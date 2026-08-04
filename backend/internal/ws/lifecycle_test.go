package ws

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/k8s-quiz/backend/internal/runner"
	"github.com/k8s-quiz/backend/pkg/models"
)

type fakeLifecycleReader struct {
	mu sync.Mutex

	bootstraps   []runner.LifecycleBootstrap
	bootstrap    int
	deltas       []runner.LifecycleDelta
	delta        int
	users        []string
	resumes      []*runner.LifecycleCursor
	cursors      []runner.LifecycleCursor
	bootstrapErr error
	deltaErr     error
	readStarted  chan struct{}
	readOnce     sync.Once
	blockDelta   chan struct{}
}

func (f *fakeLifecycleReader) BootstrapLifecycle(_ context.Context, userID string, resume *runner.LifecycleCursor) (runner.LifecycleBootstrap, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users = append(f.users, userID)
	if resume != nil {
		copy := *resume
		f.resumes = append(f.resumes, &copy)
	} else {
		f.resumes = append(f.resumes, nil)
	}
	if f.bootstrapErr != nil {
		return runner.LifecycleBootstrap{}, f.bootstrapErr
	}
	if len(f.bootstraps) == 0 {
		return runner.LifecycleBootstrap{}, nil
	}
	index := f.bootstrap
	if index >= len(f.bootstraps) {
		index = len(f.bootstraps) - 1
	} else {
		f.bootstrap++
	}
	return f.bootstraps[index], nil
}

func (f *fakeLifecycleReader) ReadLifecycleDelta(ctx context.Context, userID string, cursor runner.LifecycleCursor, _ int) (runner.LifecycleDelta, error) {
	if f.readStarted != nil {
		f.readOnce.Do(func() { close(f.readStarted) })
	}
	if f.blockDelta != nil {
		select {
		case <-ctx.Done():
			return runner.LifecycleDelta{}, ctx.Err()
		case <-f.blockDelta:
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users = append(f.users, userID)
	f.cursors = append(f.cursors, cursor)
	if f.deltaErr != nil {
		return runner.LifecycleDelta{}, f.deltaErr
	}
	if f.delta >= len(f.deltas) {
		return runner.LifecycleDelta{}, nil
	}
	delta := f.deltas[f.delta]
	f.delta++
	return delta, nil
}

func lifecycleTestSnapshot(generation, sequence uint64, status string) *runner.LifecycleSnapshot {
	return &runner.LifecycleSnapshot{
		SessionID: "22222222-2222-4222-8222-222222222222", ProblemID: "pod-crashloop",
		Generation: generation, OperationID: "op-1", Status: status,
		TimeoutAt: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC), EventSequence: sequence,
	}
}

func lifecycleTestEvent(sequence uint64, eventType string) runner.DurableEvent {
	return runner.DurableEvent{
		Sequence: sequence, Type: eventType, ReasonCode: "test", Message: eventType,
		Payload: json.RawMessage(`{"safe":true}`), CreatedAt: time.Date(2030, 1, 2, 3, 4, int(sequence), 0, time.UTC),
	}
}

func TestLifecycleEventFrameKeepsRequiredEmptyStrings(t *testing.T) {
	payload, err := json.Marshal(lifecycleEventFrame{
		Type: "lifecycle_event", Schema: lifecycleSchema,
		SessionID: "session-1", Generation: 1, EventSequence: 1,
		EventType: "ready", Payload: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	var frame map[string]any
	if err := json.Unmarshal(payload, &frame); err != nil {
		t.Fatal(err)
	}
	if reason, ok := frame["reason_code"]; !ok || reason != "" {
		t.Fatalf("reason_code = %#v, present=%v; required empty string was omitted", reason, ok)
	}
	if message, ok := frame["message"]; !ok || message != "" {
		t.Fatalf("message = %#v, present=%v; required empty string was omitted", message, ok)
	}
}

type lifecycleTestServer struct {
	server  *httptest.Server
	handler *LifecycleHandler
	cancel  context.CancelFunc
}

func newLifecycleTestServer(t *testing.T, reader LifecycleReader) lifecycleTestServer {
	t.Helper()
	gin.SetMode(gin.TestMode)
	ctx, cancel := context.WithCancel(context.Background())
	validator := &stubValidator{users: map[string]*models.User{
		"good-token": {ID: "11111111-1111-4111-8111-111111111111", Username: "tester"},
	}}
	handler := NewLifecycleHandler(reader, validator, ctx, "https://quiz.example")
	handler.subscribeGrace = 20 * time.Millisecond
	handler.pollInterval = 10 * time.Millisecond
	handler.pingInterval = time.Hour
	router := gin.New()
	router.GET("/ws/lifecycle", handler.HandleWebSocket)
	server := httptest.NewServer(router)
	t.Cleanup(func() {
		cancel()
		server.Close()
	})
	return lifecycleTestServer{server: server, handler: handler, cancel: cancel}
}

func (s lifecycleTestServer) url() string {
	return "ws" + strings.TrimPrefix(s.server.URL, "http") + "/ws/lifecycle"
}

func lifecycleHeaders(token, origin string) http.Header {
	header := http.Header{}
	if token != "" {
		header.Set("Cookie", "access_token="+token)
	}
	if origin != "" {
		header.Set("Origin", origin)
	}
	return header
}

func dialLifecycle(t *testing.T, server lifecycleTestServer, token, origin string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	conn, response, err := websocket.DefaultDialer.Dial(server.url(), lifecycleHeaders(token, origin))
	if conn != nil {
		t.Cleanup(func() { _ = conn.Close() })
	}
	return conn, response, err
}

func readLifecycleFrame(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read lifecycle frame: %v", err)
	}
	var frame map[string]any
	if err := json.Unmarshal(data, &frame); err != nil {
		t.Fatalf("decode lifecycle frame %s: %v", data, err)
	}
	return frame
}

func writeLifecycleSubscribe(t *testing.T, conn *websocket.Conn, cursor runner.LifecycleCursor) {
	t.Helper()
	if err := conn.WriteJSON(lifecycleSubscribe{Type: "lifecycle_subscribe", Cursor: &cursor}); err != nil {
		t.Fatalf("write lifecycle subscribe: %v", err)
	}
}

func TestLifecycleRejectsAuthAndOriginBeforeUpgrade(t *testing.T) {
	reader := &fakeLifecycleReader{}
	server := newLifecycleTestServer(t, reader)
	tests := []struct {
		name, token, origin string
		bearer              bool
		status              int
	}{
		{name: "missing origin", token: "good-token", status: http.StatusForbidden},
		{name: "wrong origin", token: "good-token", origin: "https://evil.example", status: http.StatusForbidden},
		{name: "missing cookie", origin: "https://quiz.example", status: http.StatusUnauthorized},
		{name: "invalid cookie", token: "bad-token", origin: "https://quiz.example", status: http.StatusUnauthorized},
		{name: "bearer is not browser auth", origin: "https://quiz.example", bearer: true, status: http.StatusUnauthorized},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			header := lifecycleHeaders(tc.token, tc.origin)
			if tc.bearer {
				header.Set("Authorization", "Bearer good-token")
			}
			conn, response, err := websocket.DefaultDialer.Dial(server.url(), header)
			if conn != nil {
				_ = conn.Close()
			}
			if err == nil || response == nil || response.StatusCode != tc.status {
				t.Fatalf("dial err=%v response=%v, want status %d", err, response, tc.status)
			}
		})
	}
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if len(reader.users) != 0 {
		t.Fatalf("rejected upgrades queried lifecycle store: %v", reader.users)
	}
}

func TestLifecycleMalformedSubscribeClosesWithoutStoreQuery(t *testing.T) {
	reader := &fakeLifecycleReader{}
	server := newLifecycleTestServer(t, reader)
	conn, _, err := dialLifecycle(t, server, "good-token", "https://quiz.example")
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"lifecycle_subscribe","cursor":{"session_id":"s","generation":0,"event_sequence":1}}`)); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("malformed subscribe left lifecycle socket open")
	}
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if len(reader.users) != 0 {
		t.Fatalf("malformed subscribe queried lifecycle store: %v", reader.users)
	}
}

func TestLifecycleReplaySnapshotThenTailOrdering(t *testing.T) {
	snapshot := lifecycleTestSnapshot(1, 3, "setting_up")
	reader := &fakeLifecycleReader{
		bootstraps: []runner.LifecycleBootstrap{{
			Snapshot: snapshot,
			Events:   []runner.DurableEvent{lifecycleTestEvent(2, "vm_created"), lifecycleTestEvent(3, "setup_running")},
		}},
		deltas: []runner.LifecycleDelta{{Events: []runner.DurableEvent{lifecycleTestEvent(4, "ready")}}},
	}
	server := newLifecycleTestServer(t, reader)
	conn, _, err := dialLifecycle(t, server, "good-token", "https://quiz.example")
	if err != nil {
		t.Fatal(err)
	}
	writeLifecycleSubscribe(t, conn, runner.LifecycleCursor{
		SessionID: snapshot.SessionID, Generation: 1, EventSequence: 1,
	})

	for index, wantType := range []string{"lifecycle_event", "lifecycle_event", "lifecycle_snapshot", "lifecycle_event"} {
		frame := readLifecycleFrame(t, conn)
		if frame["type"] != wantType {
			t.Fatalf("frame %d type=%v, want %s: %v", index, frame["type"], wantType, frame)
		}
		if wantType == "lifecycle_event" {
			wantSequence := float64(index + 2)
			if index == 3 {
				wantSequence = 4
			}
			if frame["event_sequence"] != wantSequence {
				t.Fatalf("frame %d sequence=%v, want %v", index, frame["event_sequence"], wantSequence)
			}
		}
	}

	reader.mu.Lock()
	defer reader.mu.Unlock()
	if len(reader.resumes) != 1 || reader.resumes[0] == nil || reader.resumes[0].EventSequence != 1 {
		t.Fatalf("bootstrap resumes = %+v", reader.resumes)
	}
	if len(reader.cursors) == 0 || reader.cursors[0].EventSequence != 3 {
		t.Fatalf("first tail cursors = %+v, want watermark 3", reader.cursors)
	}
}

func TestLifecycleNoSubscribeSendsAuthoritativeNullSnapshot(t *testing.T) {
	reader := &fakeLifecycleReader{bootstraps: []runner.LifecycleBootstrap{{Snapshot: nil}}}
	server := newLifecycleTestServer(t, reader)
	conn, _, err := dialLifecycle(t, server, "good-token", "https://quiz.example")
	if err != nil {
		t.Fatal(err)
	}
	frame := readLifecycleFrame(t, conn)
	if frame["type"] != "lifecycle_snapshot" || frame["session"] != nil || frame["cursor"] != nil {
		t.Fatalf("no-session snapshot = %v", frame)
	}
}

func TestLifecycleGenerationChangeSendsNewSnapshot(t *testing.T) {
	first := lifecycleTestSnapshot(1, 3, "ready")
	second := lifecycleTestSnapshot(2, 2, "queued")
	reader := &fakeLifecycleReader{
		bootstraps: []runner.LifecycleBootstrap{{Snapshot: first}},
		deltas:     []runner.LifecycleDelta{{Snapshot: second, SnapshotChanged: true}},
	}
	server := newLifecycleTestServer(t, reader)
	conn, _, err := dialLifecycle(t, server, "good-token", "https://quiz.example")
	if err != nil {
		t.Fatal(err)
	}
	if frame := readLifecycleFrame(t, conn); frame["type"] != "lifecycle_snapshot" {
		t.Fatalf("initial frame = %v", frame)
	}
	frame := readLifecycleFrame(t, conn)
	if frame["type"] != "lifecycle_snapshot" {
		t.Fatalf("generation change frame = %v", frame)
	}
	session, ok := frame["session"].(map[string]any)
	if !ok || session["generation"] != float64(2) || frame["cursor"].(map[string]any)["event_sequence"] != float64(2) {
		t.Fatalf("generation change snapshot = %v", frame)
	}
}

func TestLifecycleSnapshotIncludesLatestVerifyResult(t *testing.T) {
	snapshot := lifecycleTestSnapshot(1, 4, "ready")
	snapshot.LatestVerifyResult = &runner.LifecycleVerifyResult{Success: false, Log: "still broken"}
	reader := &fakeLifecycleReader{bootstraps: []runner.LifecycleBootstrap{{Snapshot: snapshot}}}
	server := newLifecycleTestServer(t, reader)
	conn, _, err := dialLifecycle(t, server, "good-token", "https://quiz.example")
	if err != nil {
		t.Fatal(err)
	}
	frame := readLifecycleFrame(t, conn)
	sessionFrame := frame["session"].(map[string]any)
	verifyResult := sessionFrame["latest_verify_result"].(map[string]any)
	if verifyResult["success"] != false || verifyResult["log"] != "still broken" {
		t.Fatalf("latest verify snapshot result = %v", verifyResult)
	}
}

func TestLifecycleGapForcesResyncWithoutSendingGapEvent(t *testing.T) {
	first := lifecycleTestSnapshot(1, 3, "setting_up")
	fresh := lifecycleTestSnapshot(1, 5, "ready")
	reader := &fakeLifecycleReader{
		bootstraps: []runner.LifecycleBootstrap{{Snapshot: first}, {Snapshot: fresh}},
		deltas:     []runner.LifecycleDelta{{Events: []runner.DurableEvent{lifecycleTestEvent(5, "ready")}}},
	}
	server := newLifecycleTestServer(t, reader)
	conn, _, err := dialLifecycle(t, server, "good-token", "https://quiz.example")
	if err != nil {
		t.Fatal(err)
	}
	if frame := readLifecycleFrame(t, conn); frame["type"] != "lifecycle_snapshot" {
		t.Fatalf("initial frame = %v", frame)
	}
	resync := readLifecycleFrame(t, conn)
	if resync["type"] != "lifecycle_resync_required" {
		t.Fatalf("gap frame = %v", resync)
	}
	snapshot := readLifecycleFrame(t, conn)
	if snapshot["type"] != "lifecycle_snapshot" || snapshot["cursor"].(map[string]any)["event_sequence"] != float64(5) {
		t.Fatalf("fresh snapshot = %v", snapshot)
	}
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if len(reader.resumes) < 2 || reader.resumes[1] == nil || reader.resumes[1].EventSequence != 3 {
		t.Fatalf("gap resync did not retain owned cursor: %+v", reader.resumes)
	}
}

func TestLifecycleUnavailableDeltaResyncsInsteadOfClosing(t *testing.T) {
	first := lifecycleTestSnapshot(1, 3, "setting_up")
	fresh := lifecycleTestSnapshot(1, 5, "ready")
	reader := &fakeLifecycleReader{
		bootstraps: []runner.LifecycleBootstrap{{Snapshot: first}, {Snapshot: fresh, Resync: true}},
		deltaErr:   runner.ErrLifecycleCursorUnavailable,
	}
	server := newLifecycleTestServer(t, reader)
	conn, _, err := dialLifecycle(t, server, "good-token", "https://quiz.example")
	if err != nil {
		t.Fatal(err)
	}
	if frame := readLifecycleFrame(t, conn); frame["type"] != "lifecycle_snapshot" {
		t.Fatalf("initial frame = %v", frame)
	}
	if frame := readLifecycleFrame(t, conn); frame["type"] != "lifecycle_resync_required" {
		t.Fatalf("cursor error frame = %v", frame)
	}
	if frame := readLifecycleFrame(t, conn); frame["type"] != "lifecycle_snapshot" ||
		frame["cursor"].(map[string]any)["event_sequence"] != float64(5) {
		t.Fatalf("cursor error snapshot = %v", frame)
	}
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if len(reader.resumes) < 2 || reader.resumes[1] == nil || reader.resumes[1].EventSequence != 3 {
		t.Fatalf("cursor error resync did not retain owned cursor: %+v", reader.resumes)
	}
}

func TestLifecycleNonOwnedCursorReturnsGenericError(t *testing.T) {
	reader := &fakeLifecycleReader{bootstrapErr: runner.ErrLifecycleNotFoundOrForbidden}
	server := newLifecycleTestServer(t, reader)
	conn, _, err := dialLifecycle(t, server, "good-token", "https://quiz.example")
	if err != nil {
		t.Fatal(err)
	}
	writeLifecycleSubscribe(t, conn, runner.LifecycleCursor{SessionID: "other", Generation: 1, EventSequence: 1})
	frame := readLifecycleFrame(t, conn)
	if frame["type"] != "error" || frame["error"] != "not_found_or_forbidden" || len(frame) != 2 {
		t.Fatalf("non-owned error leaked fields: %v", frame)
	}
}

func TestLifecycleSecondConnectionReplacesFirst(t *testing.T) {
	reader := &fakeLifecycleReader{bootstraps: []runner.LifecycleBootstrap{{Snapshot: lifecycleTestSnapshot(1, 1, "queued")}}}
	server := newLifecycleTestServer(t, reader)
	first, _, err := dialLifecycle(t, server, "good-token", "https://quiz.example")
	if err != nil {
		t.Fatal(err)
	}
	_ = readLifecycleFrame(t, first)
	second, _, err := dialLifecycle(t, server, "good-token", "https://quiz.example")
	if err != nil {
		t.Fatal(err)
	}
	_ = readLifecycleFrame(t, second)
	_ = first.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := first.ReadMessage(); err == nil {
		t.Fatal("replaced lifecycle connection remained open")
	} else if closeErr, ok := err.(*websocket.CloseError); !ok || closeErr.Code != CloseConnectionReplaced {
		t.Fatalf("replaced lifecycle close=%v, want code %d", err, CloseConnectionReplaced)
	}
}

func TestLifecycleShutdownCancelsBlockedPoll(t *testing.T) {
	reader := &fakeLifecycleReader{
		bootstraps:  []runner.LifecycleBootstrap{{Snapshot: lifecycleTestSnapshot(1, 1, "queued")}},
		readStarted: make(chan struct{}), blockDelta: make(chan struct{}),
	}
	server := newLifecycleTestServer(t, reader)
	conn, _, err := dialLifecycle(t, server, "good-token", "https://quiz.example")
	if err != nil {
		t.Fatal(err)
	}
	_ = readLifecycleFrame(t, conn)
	select {
	case <-reader.readStarted:
	case <-time.After(time.Second):
		t.Fatal("lifecycle poll did not start")
	}
	server.cancel()
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("lifecycle shutdown left socket open")
	}
}

func TestLifecycleAccessTokenExpiryClosesSocket(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader := &fakeLifecycleReader{bootstraps: []runner.LifecycleBootstrap{{Snapshot: lifecycleTestSnapshot(1, 1, "ready")}}}
	validator := &stubValidator{
		users: map[string]*models.User{
			"short-token": {ID: "11111111-1111-4111-8111-111111111111", Username: "tester"},
		},
		expiresAt: map[string]time.Time{"short-token": time.Now().Add(250 * time.Millisecond)},
	}
	handler := NewLifecycleHandler(reader, validator, ctx, "https://quiz.example")
	handler.subscribeGrace = 10 * time.Millisecond
	handler.pollInterval = time.Hour
	handler.pingInterval = time.Hour
	router := gin.New()
	router.GET("/ws/lifecycle", handler.HandleWebSocket)
	server := httptest.NewServer(router)
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/lifecycle"
	conn, _, err := websocket.DefaultDialer.Dial(url, lifecycleHeaders("short-token", "https://quiz.example"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("initial lifecycle snapshot before expiry: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("lifecycle socket outlived verified access-token expiry")
	}
}

func TestLifecycleStoreFailureClosesWithoutInternalErrorPayload(t *testing.T) {
	reader := &fakeLifecycleReader{bootstrapErr: errors.New("database host secret: do not leak")}
	server := newLifecycleTestServer(t, reader)
	conn, _, err := dialLifecycle(t, server, "good-token", "https://quiz.example")
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	_, data, err := conn.ReadMessage()
	if err == nil || strings.Contains(string(data), "database host secret") {
		t.Fatalf("store failure err=%v data=%q", err, data)
	}
}
