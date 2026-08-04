package ws

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/k8s-quiz/backend/internal/runner"
	"github.com/k8s-quiz/backend/pkg/models"
)

// ---- stubs ----

type stubValidator struct {
	users     map[string]*models.User
	expiresAt map[string]time.Time
}

func (v *stubValidator) ValidateAccessToken(token string) (*models.User, error) {
	u, _, err := v.ValidateAccessTokenWithExpiry(token)
	return u, err
}

func (v *stubValidator) ValidateAccessTokenWithExpiry(token string) (*models.User, time.Time, error) {
	u, ok := v.users[token]
	if !ok {
		return nil, time.Time{}, errors.New("invalid token")
	}
	expiresAt := time.Now().Add(time.Hour)
	if v.expiresAt != nil {
		if configured, exists := v.expiresAt[token]; exists {
			expiresAt = configured
		}
	}
	if !expiresAt.After(time.Now()) {
		return nil, time.Time{}, errors.New("expired token")
	}
	return u, expiresAt, nil
}

// fakeTerminalSession pipes terminal output (test→server) and forwarded input
// (server→test) separately so tests can observe both directions.
type fakeTerminalSession struct {
	outR      *io.PipeReader
	outW      *io.PipeWriter
	inR       *io.PipeReader
	inW       *io.PipeWriter
	mu        sync.Mutex
	closeOnce sync.Once
	closed    chan struct{}
	resizes   [][2]int
}

type immediateTerminalSession struct {
	data      []byte
	err       error
	readOnce  sync.Once
	closeOnce sync.Once
	closed    chan struct{}
}

type mutationCountingTerminal struct {
	mu        sync.Mutex
	writes    int
	resizes   int
	closeOnce sync.Once
	closed    chan struct{}
}

func newMutationCountingTerminal() *mutationCountingTerminal {
	return &mutationCountingTerminal{closed: make(chan struct{})}
}

func (t *mutationCountingTerminal) Read([]byte) (int, error) {
	<-t.closed
	return 0, io.EOF
}

func (t *mutationCountingTerminal) Write(p []byte) (int, error) {
	t.mu.Lock()
	t.writes++
	t.mu.Unlock()
	return len(p), nil
}

func (t *mutationCountingTerminal) Resize(int, int) error {
	t.mu.Lock()
	t.resizes++
	t.mu.Unlock()
	return nil
}

func (t *mutationCountingTerminal) Close() error {
	t.closeOnce.Do(func() { close(t.closed) })
	return nil
}

func (t *mutationCountingTerminal) mutations() (int, int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.writes, t.resizes
}

func newImmediateTerminalSession(data string, err error) *immediateTerminalSession {
	return &immediateTerminalSession{data: []byte(data), err: err, closed: make(chan struct{})}
}

func (f *immediateTerminalSession) Read(p []byte) (int, error) {
	n := 0
	err := io.EOF
	f.readOnce.Do(func() {
		n = copy(p, f.data)
		err = f.err
	})
	return n, err
}
func (*immediateTerminalSession) Write(p []byte) (int, error) { return len(p), nil }
func (f *immediateTerminalSession) Close() error {
	f.closeOnce.Do(func() { close(f.closed) })
	return nil
}
func (*immediateTerminalSession) Resize(int, int) error { return nil }

type blockingTerminalOpener struct {
	started chan struct{}
	done    chan struct{}
}

func (m *blockingTerminalOpener) OpenTerminal(ctx context.Context, _ runner.OpenTerminalRequest) (runner.TerminalSession, error) {
	close(m.started)
	<-ctx.Done()
	close(m.done)
	return nil, ctx.Err()
}

type delayedTerminalOpener struct {
	terminal runner.TerminalSession
	started  chan struct{}
	release  chan struct{}
}

func (m *delayedTerminalOpener) OpenTerminal(ctx context.Context, _ runner.OpenTerminalRequest) (runner.TerminalSession, error) {
	close(m.started)
	select {
	case <-m.release:
		return m.terminal, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func newFakeTerminalSession() *fakeTerminalSession {
	outR, outW := io.Pipe()
	inR, inW := io.Pipe()
	return &fakeTerminalSession{outR: outR, outW: outW, inR: inR, inW: inW, closed: make(chan struct{})}
}

func (f *fakeTerminalSession) Read(p []byte) (int, error)  { return f.outR.Read(p) }
func (f *fakeTerminalSession) Write(p []byte) (int, error) { return f.inW.Write(p) }
func (f *fakeTerminalSession) Close() error {
	f.closeOnce.Do(func() {
		f.outR.Close()
		f.outW.Close()
		f.inR.Close()
		f.inW.Close()
		close(f.closed)
	})
	return nil
}
func (f *fakeTerminalSession) Resize(cols, rows int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resizes = append(f.resizes, [2]int{cols, rows})
	return nil
}
func (f *fakeTerminalSession) lastResize() ([2]int, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.resizes) == 0 {
		return [2]int{}, false
	}
	return f.resizes[len(f.resizes)-1], true
}

type stubTerminalOpener struct {
	terminal runner.TerminalSession
	err      error
	mu       sync.Mutex
	requests []runner.OpenTerminalRequest
}

type sequencedTerminalOpener struct {
	mu        sync.Mutex
	terminals []runner.TerminalSession
	errors    []error
	calls     int
	requests  []runner.OpenTerminalRequest
}

func (m *sequencedTerminalOpener) OpenTerminal(_ context.Context, request runner.OpenTerminalRequest) (runner.TerminalSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	index := m.calls
	m.calls++
	m.requests = append(m.requests, request)
	if index < len(m.errors) && m.errors[index] != nil {
		return nil, m.errors[index]
	}
	if index >= len(m.terminals) || m.terminals[index] == nil {
		return nil, errors.New("missing terminal response")
	}
	return m.terminals[index], nil
}

func (m *stubTerminalOpener) OpenTerminal(ctx context.Context, req runner.OpenTerminalRequest) (runner.TerminalSession, error) {
	m.mu.Lock()
	m.requests = append(m.requests, req)
	m.mu.Unlock()
	if m.err != nil {
		return nil, m.err
	}
	return m.terminal, nil
}

const testFrontendURL = "http://terminal.test"

func testAllocation() runner.AllocationRef {
	return runner.AllocationRef{
		ID:       "allocation-1",
		Session:  runner.SessionRef{SessionID: "session-1", Generation: 1},
		Provider: runner.ProviderLocalDocker,
	}
}

func sessionTarget(found bool) func(string, runner.SessionRef) (runner.AllocationRef, bool) {
	return func(_ string, expected runner.SessionRef) (runner.AllocationRef, bool) {
		allocation := testAllocation()
		if !found || allocation.Session != expected {
			return runner.AllocationRef{}, false
		}
		return allocation, true
	}
}

func allowTerminalCommit(context.Context, string, runner.AllocationRef) (func(), bool) {
	return func() {}, true
}

func TestNewTerminalHandlerRequiresCommitAuthorizer(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("missing terminal commit authorizer did not fail closed")
		}
	}()
	NewTerminalHandler(NewHub(), &stubValidator{}, &stubTerminalOpener{}, sessionTarget(true), nil, func() bool { return true }, context.Background(), testFrontendURL)
}

func setupTerminalServer(t *testing.T, getSession func(string, runner.SessionRef) (runner.AllocationRef, bool), term runner.TerminalSession) (*httptest.Server, *Hub) {
	t.Helper()
	hub := NewHub()
	go hub.Run()
	validator := &stubValidator{users: map[string]*models.User{
		"good-token": {ID: "u1", Username: "tester", Role: models.RoleUser},
	}}
	h := NewTerminalHandler(hub, validator, &stubTerminalOpener{terminal: term}, getSession, allowTerminalCommit, func() bool { return true }, context.Background(), testFrontendURL)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/ws/terminal", h.HandleWebSocket)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, hub
}

func wsURL(srv *httptest.Server) string {
	return "ws" + srv.URL[len("http"):] + "/ws/terminal?session_id=session-1&generation=1"
}

func dialTerminal(t *testing.T, srv *httptest.Server, header http.Header) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL(srv), header)
	if conn != nil {
		t.Cleanup(func() { conn.Close() })
	}
	return conn, resp, err
}

func cookieHeader(value string) http.Header {
	h := http.Header{}
	h.Set("Cookie", "access_token="+value)
	return h
}

// readOne reads a single WS message with a timeout (non-fatal).
func readOne(conn *websocket.Conn, timeout time.Duration) (Message, error) {
	conn.SetReadDeadline(time.Now().Add(timeout))
	_, data, err := conn.ReadMessage()
	if err != nil {
		return Message{}, err
	}
	var m Message
	if err := json.Unmarshal(data, &m); err != nil {
		return Message{}, err
	}
	return m, nil
}

func attachTerminal(conn *websocket.Conn, timeout time.Duration) (Message, error) {
	attached, err := readOne(conn, timeout)
	if err != nil {
		return Message{}, err
	}
	if attached.Type != MsgTerminalAttached || attached.AttachNonce == "" {
		return Message{}, errors.New("terminal did not return an exact attach nonce")
	}
	if err := conn.WriteJSON(Message{Type: MsgTerminalReady, AttachNonce: attached.AttachNonce}); err != nil {
		return Message{}, err
	}
	return attached, nil
}

func waitForHubOwner(t *testing.T, hub *Hub, userID string, predicate func(*Client) bool) *Client {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		owner := hub.GetClient(userID)
		if predicate(owner) {
			return owner
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("Hub owner for %s did not reach expected state", userID)
	return nil
}

// ---- tests ----

// SEC3-7 / checkOrigin: a cross-origin upgrade is rejected with 403.
func TestTerminalWrongOriginRejected(t *testing.T) {
	srv, _ := setupTerminalServer(t, sessionTarget(true), newFakeTerminalSession())
	h := http.Header{}
	h.Set("Origin", "http://evil.com")
	conn, resp, err := dialTerminal(t, srv, h)
	if err == nil {
		t.Fatal("expected upgrade rejection for wrong origin")
	}
	if conn != nil {
		t.Error("expected no connection")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403, got resp=%v", resp)
	}
}

func TestTerminalBrowserInvalidCookieRejectedBeforeUpgrade(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	opener := &stubTerminalOpener{terminal: newFakeTerminalSession()}
	validator := &stubValidator{users: map[string]*models.User{"good-token": {ID: "u1"}}}
	handler := NewTerminalHandler(hub, validator, opener, sessionTarget(true), allowTerminalCommit, func() bool { return true }, context.Background(), testFrontendURL)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/ws/terminal", handler.HandleWebSocket)
	srv := httptest.NewServer(r)
	defer srv.Close()

	for _, token := range []string{"", "bad-token"} {
		header := http.Header{}
		header.Set("Origin", testFrontendURL)
		if token != "" {
			header.Set("Cookie", "access_token="+token)
		}
		conn, response, err := websocket.DefaultDialer.Dial(wsURL(srv), header)
		if conn != nil {
			conn.Close()
		}
		if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("browser token %q dial err=%v response=%v, want 401", token, err, response)
		}
	}
	opener.mu.Lock()
	requests := len(opener.requests)
	opener.mu.Unlock()
	if requests != 0 {
		t.Fatalf("rejected browser auth opened provider %d time(s)", requests)
	}
}

func TestTerminalNotReadyRejectedBeforeUpgrade(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	validator := &stubValidator{users: map[string]*models.User{"good-token": {ID: "u1"}}}
	opener := &stubTerminalOpener{terminal: newFakeTerminalSession()}
	handler := NewTerminalHandler(hub, validator, opener, sessionTarget(true), allowTerminalCommit, func() bool { return false }, context.Background(), testFrontendURL)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/ws/terminal", handler.HandleWebSocket)
	srv := httptest.NewServer(r)
	defer srv.Close()

	conn, response, err := websocket.DefaultDialer.Dial(wsURL(srv), cookieHeader("good-token"))
	if conn != nil {
		conn.Close()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("not-ready terminal dial err=%v response=%v", err, response)
	}
	opener.mu.Lock()
	requests := len(opener.requests)
	opener.mu.Unlock()
	if requests != 0 {
		t.Fatalf("not-ready terminal reached provider %d time(s)", requests)
	}
}

func TestTerminalAuthorityCancellationClosesLegacyAuthWait(t *testing.T) {
	authorityCtx, cancelAuthority := context.WithCancel(context.Background())
	hub := NewHub()
	go hub.Run()
	validator := &stubValidator{users: map[string]*models.User{"good-token": {ID: "u1"}}}
	opener := &stubTerminalOpener{terminal: newFakeTerminalSession()}
	handler := NewTerminalHandler(hub, validator, opener, sessionTarget(true), allowTerminalCommit, func() bool { return true }, authorityCtx, testFrontendURL)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/ws/terminal", handler.HandleWebSocket)
	srv := httptest.NewServer(r)
	defer srv.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(srv), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	cancelAuthority()
	conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("authority cancellation left legacy auth socket open")
	}
	opener.mu.Lock()
	requests := len(opener.requests)
	opener.mu.Unlock()
	if requests != 0 {
		t.Fatalf("cancelled legacy auth reached provider %d time(s)", requests)
	}
}

// No cookie + first message is not auth → error + close, no output.
func TestTerminalNoAuthClosed(t *testing.T) {
	srv, _ := setupTerminalServer(t, sessionTarget(true), newFakeTerminalSession())
	conn, _, err := dialTerminal(t, srv, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	conn.WriteJSON(Message{Type: MsgInput, Data: "kubectl get pods"})

	m, err := readOne(conn, 2*time.Second)
	if err != nil {
		t.Fatalf("expected an error message before close, got %v", err)
	}
	if m.Type != MsgError || m.Message != "authentication required" {
		t.Errorf("expected authentication required error, got %+v", m)
	}
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Error("expected connection closed after auth failure")
	}
}

// Cookie-authed but no active session → error + close.
func TestTerminalNoSessionClosed(t *testing.T) {
	srv, _ := setupTerminalServer(t, sessionTarget(false), newFakeTerminalSession())
	conn, _, err := dialTerminal(t, srv, cookieHeader("good-token"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	m, err := readOne(conn, 2*time.Second)
	if err != nil {
		t.Fatalf("expected error message, got %v", err)
	}
	if m.Type != MsgError || m.Message != "session identity is not the current ready terminal target" {
		t.Errorf("expected exact session identity error, got %+v", m)
	}
}

func TestTerminalMissingSessionIdentityRejectedBeforeUpgrade(t *testing.T) {
	srv, _ := setupTerminalServer(t, sessionTarget(true), newFakeTerminalSession())
	url := "ws" + srv.URL[len("http"):] + "/ws/terminal"
	conn, response, err := websocket.DefaultDialer.Dial(url, cookieHeader("good-token"))
	if conn != nil {
		conn.Close()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing identity dial err=%v response=%v", err, response)
	}
}

func TestTerminalMismatchedSessionIdentityNeverOpensProvider(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	opener := &stubTerminalOpener{terminal: newFakeTerminalSession()}
	validator := &stubValidator{users: map[string]*models.User{"good-token": {ID: "u1"}}}
	handler := NewTerminalHandler(hub, validator, opener, sessionTarget(true), allowTerminalCommit, func() bool { return true }, context.Background(), testFrontendURL)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/ws/terminal", handler.HandleWebSocket)
	srv := httptest.NewServer(r)
	defer srv.Close()
	url := "ws" + srv.URL[len("http"):] + "/ws/terminal?session_id=stale-session&generation=1"
	conn, _, err := websocket.DefaultDialer.Dial(url, cookieHeader("good-token"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	message, err := readOne(conn, 2*time.Second)
	if err != nil || message.Type != MsgError {
		t.Fatalf("mismatched identity response=%+v err=%v", message, err)
	}
	opener.mu.Lock()
	requests := len(opener.requests)
	opener.mu.Unlock()
	if requests != 0 {
		t.Fatalf("mismatched terminal identity opened provider %d time(s)", requests)
	}
}

// Cookie-authed with a session: no auth message needed; output flows, input
// is forwarded, and resize is clamped (SEC3-7).
func TestTerminalCookieAuthFullDuplex(t *testing.T) {
	fake := newFakeTerminalSession()
	srv, _ := setupTerminalServer(t, sessionTarget(true), fake)
	conn, _, err := dialTerminal(t, srv, cookieHeader("good-token"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	attached, err := attachTerminal(conn, 2*time.Second)
	if err != nil || attached.Type != MsgTerminalAttached || attached.SessionID != "session-1" || attached.Generation != 1 {
		t.Fatalf("first terminal frame = %+v err=%v, want exact attached ack", attached, err)
	}

	// Output injected by the "container" reaches the browser client after ACK.
	go fake.outW.Write([]byte("hello from k3s"))
	m, err := readOne(conn, 2*time.Second)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if m.Type != MsgOutput || m.Data != "hello from k3s" {
		t.Errorf("expected output message, got %+v", m)
	}

	// Client input is forwarded to the container.
	conn.WriteJSON(Message{Type: MsgInput, Data: "kubectl get nodes", AttachNonce: attached.AttachNonce})
	buf := make([]byte, 64)
	got := make(chan string, 1)
	go func() { n, _ := fake.inR.Read(buf); got <- string(buf[:n]) }()
	select {
	case s := <-got:
		if s != "kubectl get nodes" {
			t.Errorf("expected forwarded input, got %q", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("input not forwarded to terminal")
	}

	// Absurd resize requests are clamped to sane bounds (SEC3-7).
	conn.WriteJSON(Message{Type: MsgResize, Cols: 9999, Rows: 0, AttachNonce: attached.AttachNonce})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if r, ok := fake.lastResize(); ok {
			if r != [2]int{500, 1} {
				t.Errorf("expected clamped resize {500 1}, got %v", r)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("resize not forwarded")
}

func TestTerminalCancellationAfterReceiveRejectsQueuedInputAndResize(t *testing.T) {
	for _, message := range []Message{
		{Type: MsgInput, Data: "must-not-run"},
		{Type: MsgResize, Cols: 120, Rows: 40},
	} {
		t.Run(string(message.Type), func(t *testing.T) {
			hub := NewHub()
			go hub.Run()
			terminal := newMutationCountingTerminal()
			validator := &stubValidator{users: map[string]*models.User{"good-token": {ID: "u1"}}}
			authorityCtx, cancelAuthority := context.WithCancel(context.Background())
			handler := NewTerminalHandler(hub, validator, &stubTerminalOpener{terminal: terminal}, sessionTarget(true), allowTerminalCommit, func() bool { return true }, authorityCtx, testFrontendURL)
			mutationReceived := make(chan struct{})
			handler.beforeTerminalMutation = func(_ context.Context, got Message) {
				if got.Type == message.Type {
					cancelAuthority()
					close(mutationReceived)
				}
			}
			gin.SetMode(gin.TestMode)
			router := gin.New()
			router.GET("/ws/terminal", handler.HandleWebSocket)
			server := httptest.NewServer(router)
			defer server.Close()

			conn, _, err := websocket.DefaultDialer.Dial(wsURL(server), cookieHeader("good-token"))
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			attached, err := attachTerminal(conn, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			message.AttachNonce = attached.AttachNonce
			if err := conn.WriteJSON(message); err != nil {
				t.Fatal(err)
			}
			select {
			case <-mutationReceived:
			case <-time.After(time.Second):
				t.Fatal("terminal mutation frame was not received")
			}
			select {
			case <-terminal.closed:
			case <-time.After(time.Second):
				t.Fatal("cancelled terminal was not closed")
			}
			writes, resizes := terminal.mutations()
			if writes != 0 || resizes != 0 {
				t.Fatalf("cancelled frame mutated provider writes=%d resizes=%d", writes, resizes)
			}
		})
	}
}

func TestTerminalImmediateEOFStillSendsAttachedBeforeFinalOutput(t *testing.T) {
	terminal := newImmediateTerminalSession("final output", io.EOF)
	srv, _ := setupTerminalServer(t, sessionTarget(true), terminal)
	conn, _, err := dialTerminal(t, srv, cookieHeader("good-token"))
	if err != nil {
		t.Fatal(err)
	}
	attached, err := attachTerminal(conn, 2*time.Second)
	if err != nil || attached.Type != MsgTerminalAttached {
		t.Fatalf("first frame=%+v err=%v, want attached", attached, err)
	}
	output, err := readOne(conn, 2*time.Second)
	if err != nil || output.Type != MsgOutput || output.Data != "final output" {
		t.Fatalf("second frame=%+v err=%v, want final output", output, err)
	}
}

func TestTerminalDiscardsInputSentBeforeAttachedAck(t *testing.T) {
	terminal := newFakeTerminalSession()
	opener := &delayedTerminalOpener{
		terminal: terminal,
		started:  make(chan struct{}),
		release:  make(chan struct{}),
	}
	hub := NewHub()
	go hub.Run()
	validator := &stubValidator{users: map[string]*models.User{"good-token": {ID: "u1"}}}
	handler := NewTerminalHandler(hub, validator, opener, sessionTarget(true), allowTerminalCommit, func() bool { return true }, context.Background(), testFrontendURL)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/ws/terminal", handler.HandleWebSocket)
	srv := httptest.NewServer(r)
	defer srv.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(srv), cookieHeader("good-token"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	select {
	case <-opener.started:
	case <-time.After(time.Second):
		t.Fatal("terminal open did not start")
	}
	if err := conn.WriteJSON(Message{Type: MsgInput, Data: "pre-ack"}); err != nil {
		t.Fatal(err)
	}
	close(opener.release)
	attached, err := readOne(conn, 2*time.Second)
	if err != nil || attached.Type != MsgTerminalAttached || attached.AttachNonce == "" {
		t.Fatalf("attach=%+v err=%v", attached, err)
	}
	if err := conn.WriteJSON(Message{Type: MsgTerminalReady, AttachNonce: "wrong-nonce"}); err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteJSON(Message{Type: MsgInput, Data: "still-pre-ready", AttachNonce: attached.AttachNonce}); err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteJSON(Message{Type: MsgTerminalReady, AttachNonce: attached.AttachNonce}); err != nil {
		t.Fatal(err)
	}

	readResult := make(chan string, 1)
	go func() {
		buf := make([]byte, 32)
		n, _ := terminal.inR.Read(buf)
		readResult <- string(buf[:n])
	}()
	select {
	case got := <-readResult:
		t.Fatalf("pre-ACK input reached terminal: %q", got)
	case <-time.After(50 * time.Millisecond):
	}
	if err := conn.WriteJSON(Message{Type: MsgInput, Data: "wrong-nonce", AttachNonce: "wrong-nonce"}); err != nil {
		t.Fatal(err)
	}
	if err := conn.WriteJSON(Message{Type: MsgInput, Data: "post-ack", AttachNonce: attached.AttachNonce}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-readResult:
		if got != "post-ack" {
			t.Fatalf("terminal input=%q, want post-ack", got)
		}
	case <-time.After(time.Second):
		t.Fatal("post-ACK input did not reach terminal")
	}
}

func TestTerminalBrowserDisconnectClosesRunnerLease(t *testing.T) {
	fake := newFakeTerminalSession()
	srv, _ := setupTerminalServer(t, sessionTarget(true), fake)
	conn, _, err := dialTerminal(t, srv, cookieHeader("good-token"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	// The attach ACK proves the read pump opened the Runner terminal lease.
	if attached, err := attachTerminal(conn, 2*time.Second); err != nil || attached.Type != MsgTerminalAttached {
		t.Fatalf("wait for terminal open: %v", err)
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close browser socket: %v", err)
	}
	select {
	case <-fake.closed:
	case <-time.After(time.Second):
		t.Fatal("browser disconnect did not close the Runner terminal lease")
	}
}

// Legacy first-message auth (non-browser clients) still works.
func TestTerminalLegacyAuthMessage(t *testing.T) {
	fake := newFakeTerminalSession()
	srv, _ := setupTerminalServer(t, sessionTarget(true), fake)
	conn, _, err := dialTerminal(t, srv, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	conn.WriteJSON(Message{Type: MsgAuth, Token: "good-token"})
	attached, err := attachTerminal(conn, 2*time.Second)
	if err != nil || attached.Type != MsgTerminalAttached {
		t.Fatalf("legacy first terminal frame = %+v err=%v, want attached", attached, err)
	}
	go fake.outW.Write([]byte("legacy ok"))
	m, err := readOne(conn, 2*time.Second)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if m.Type != MsgOutput || m.Data != "legacy ok" {
		t.Errorf("expected output after legacy auth, got %+v", m)
	}
}

func TestTerminalOpenFailureSendsNoAttachedAck(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	validator := &stubValidator{users: map[string]*models.User{"good-token": {ID: "u1"}}}
	opener := &stubTerminalOpener{err: errors.New("provider unavailable")}
	handler := NewTerminalHandler(hub, validator, opener, sessionTarget(true), allowTerminalCommit, func() bool { return true }, context.Background(), testFrontendURL)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/ws/terminal", handler.HandleWebSocket)
	srv := httptest.NewServer(r)
	defer srv.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(srv), cookieHeader("good-token"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	message, err := readOne(conn, 2*time.Second)
	if err != nil || message.Type != MsgError {
		t.Fatalf("open failure frame=%+v err=%v", message, err)
	}
	if message.Type == MsgTerminalAttached {
		t.Fatal("provider open failure emitted terminal_attached")
	}
}

func TestTerminalOpenFailureDoesNotReplaceExistingConnection(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	firstTerminal := newFakeTerminalSession()
	opener := &sequencedTerminalOpener{
		terminals: []runner.TerminalSession{firstTerminal, nil},
		errors:    []error{nil, errors.New("provider unavailable")},
	}
	validator := &stubValidator{users: map[string]*models.User{"good-token": {ID: "u1"}}}
	handler := NewTerminalHandler(hub, validator, opener, sessionTarget(true), allowTerminalCommit, func() bool { return true }, context.Background(), testFrontendURL)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/ws/terminal", handler.HandleWebSocket)
	srv := httptest.NewServer(r)
	defer srv.Close()

	first, _, err := websocket.DefaultDialer.Dial(wsURL(srv), cookieHeader("good-token"))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if attached, err := attachTerminal(first, 2*time.Second); err != nil || attached.Type != MsgTerminalAttached {
		t.Fatalf("first terminal attach=%+v err=%v", attached, err)
	}
	existing := waitForHubOwner(t, hub, "u1", func(client *Client) bool { return client != nil })

	second, _, err := websocket.DefaultDialer.Dial(wsURL(srv), cookieHeader("good-token"))
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if message, err := readOne(second, 2*time.Second); err != nil || message.Type != MsgError {
		t.Fatalf("failed replacement frame=%+v err=%v", message, err)
	}
	if current := hub.GetClient("u1"); current != existing {
		t.Fatal("failed replacement evicted the existing terminal")
	}
	opener.mu.Lock()
	if len(opener.requests) != 2 || opener.requests[1].ReplacesLeaseID != opener.requests[0].LeaseID ||
		opener.requests[1].ReplacesLeaseID == "" {
		t.Fatalf("replacement request did not bind prior lease: %+v", opener.requests)
	}
	opener.mu.Unlock()

	go firstTerminal.outW.Write([]byte("still connected"))
	if message, err := readOne(first, 2*time.Second); err != nil || message.Type != MsgOutput || message.Data != "still connected" {
		t.Fatalf("existing terminal after failed replacement=%+v err=%v", message, err)
	}
}

func TestTerminalCandidateDisconnectCancelsOpenAndPreservesExistingConnection(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	firstTerminal := newFakeTerminalSession()
	validator := &stubValidator{users: map[string]*models.User{"good-token": {ID: "u1"}}}
	firstHandler := NewTerminalHandler(hub, validator, &stubTerminalOpener{terminal: firstTerminal}, sessionTarget(true), allowTerminalCommit, func() bool { return true }, context.Background(), testFrontendURL)
	gin.SetMode(gin.TestMode)
	firstRouter := gin.New()
	firstRouter.GET("/ws/terminal", firstHandler.HandleWebSocket)
	firstServer := httptest.NewServer(firstRouter)
	defer firstServer.Close()

	first, _, err := websocket.DefaultDialer.Dial(wsURL(firstServer), cookieHeader("good-token"))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if attached, err := attachTerminal(first, 2*time.Second); err != nil || attached.Type != MsgTerminalAttached {
		t.Fatalf("first terminal attach=%+v err=%v", attached, err)
	}
	existing := waitForHubOwner(t, hub, "u1", func(client *Client) bool { return client != nil })

	blocking := &blockingTerminalOpener{started: make(chan struct{}), done: make(chan struct{})}
	candidateHandler := NewTerminalHandler(hub, validator, blocking, sessionTarget(true), allowTerminalCommit, func() bool { return true }, context.Background(), testFrontendURL)
	candidateRouter := gin.New()
	candidateRouter.GET("/ws/terminal", candidateHandler.HandleWebSocket)
	candidateServer := httptest.NewServer(candidateRouter)
	defer candidateServer.Close()
	candidate, _, err := websocket.DefaultDialer.Dial(wsURL(candidateServer), cookieHeader("good-token"))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-blocking.started:
	case <-time.After(time.Second):
		t.Fatal("candidate open did not start")
	}
	_ = candidate.Close()
	select {
	case <-blocking.done:
	case <-time.After(time.Second):
		t.Fatal("candidate disconnect did not cancel provider open")
	}
	if current := hub.GetClient("u1"); current != existing {
		t.Fatal("disconnected candidate changed existing Hub ownership")
	}
	go firstTerminal.outW.Write([]byte("old still alive"))
	if output, err := readOne(first, 2*time.Second); err != nil || output.Type != MsgOutput || output.Data != "old still alive" {
		t.Fatalf("existing output after candidate disconnect=%+v err=%v", output, err)
	}
}

func TestTerminalAuthorityChangeDuringOpenPreservesExistingConnection(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	validator := &stubValidator{users: map[string]*models.User{"good-token": {ID: "u1"}}}
	firstTerminal := newFakeTerminalSession()
	firstHandler := NewTerminalHandler(hub, validator, &stubTerminalOpener{terminal: firstTerminal}, sessionTarget(true), allowTerminalCommit, func() bool { return true }, context.Background(), testFrontendURL)
	gin.SetMode(gin.TestMode)
	firstRouter := gin.New()
	firstRouter.GET("/ws/terminal", firstHandler.HandleWebSocket)
	firstServer := httptest.NewServer(firstRouter)
	defer firstServer.Close()

	first, _, err := websocket.DefaultDialer.Dial(wsURL(firstServer), cookieHeader("good-token"))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := attachTerminal(first, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	existing := waitForHubOwner(t, hub, "u1", func(client *Client) bool { return client != nil })

	secondTerminal := newFakeTerminalSession()
	opener := &delayedTerminalOpener{terminal: secondTerminal, started: make(chan struct{}), release: make(chan struct{})}
	var targetMu sync.RWMutex
	targetCurrent := true
	getTarget := func(_ string, expected runner.SessionRef) (runner.AllocationRef, bool) {
		targetMu.RLock()
		current := targetCurrent
		targetMu.RUnlock()
		if !current || expected != testAllocation().Session {
			return runner.AllocationRef{}, false
		}
		return testAllocation(), true
	}
	candidateHandler := NewTerminalHandler(hub, validator, opener, getTarget, allowTerminalCommit, func() bool { return true }, context.Background(), testFrontendURL)
	candidateRouter := gin.New()
	candidateRouter.GET("/ws/terminal", candidateHandler.HandleWebSocket)
	candidateServer := httptest.NewServer(candidateRouter)
	defer candidateServer.Close()
	candidate, _, err := websocket.DefaultDialer.Dial(wsURL(candidateServer), cookieHeader("good-token"))
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Close()
	select {
	case <-opener.started:
	case <-time.After(time.Second):
		t.Fatal("candidate provider open did not start")
	}
	targetMu.Lock()
	targetCurrent = false
	targetMu.Unlock()
	close(opener.release)
	if message, err := readOne(candidate, 2*time.Second); err != nil || message.Type != MsgError || message.Message != "terminal session authority changed" {
		t.Fatalf("stale candidate response=%+v err=%v", message, err)
	}
	select {
	case <-secondTerminal.closed:
	case <-time.After(time.Second):
		t.Fatal("stale candidate Runner lease remained open")
	}
	if current := hub.GetClient("u1"); current != existing {
		t.Fatal("stale candidate replaced the established terminal")
	}
	go func() { _, _ = firstTerminal.outW.Write([]byte("old authority remains")) }()
	if output, err := readOne(first, 2*time.Second); err != nil || output.Type != MsgOutput || output.Data != "old authority remains" {
		t.Fatalf("established terminal after authority change=%+v err=%v", output, err)
	}
}

func TestTerminalCommitLeaseDenialPreservesExistingConnection(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	validator := &stubValidator{users: map[string]*models.User{"good-token": {ID: "u1"}}}
	firstTerminal := newFakeTerminalSession()
	firstHandler := NewTerminalHandler(hub, validator, &stubTerminalOpener{terminal: firstTerminal}, sessionTarget(true), allowTerminalCommit, func() bool { return true }, context.Background(), testFrontendURL)
	gin.SetMode(gin.TestMode)
	firstRouter := gin.New()
	firstRouter.GET("/ws/terminal", firstHandler.HandleWebSocket)
	firstServer := httptest.NewServer(firstRouter)
	defer firstServer.Close()

	first, _, err := websocket.DefaultDialer.Dial(wsURL(firstServer), cookieHeader("good-token"))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := attachTerminal(first, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	existing := waitForHubOwner(t, hub, "u1", func(client *Client) bool { return client != nil })

	candidateTerminal := newFakeTerminalSession()
	denied := make(chan struct{})
	denyCommit := func(_ context.Context, userID string, allocation runner.AllocationRef) (func(), bool) {
		if userID != "u1" || allocation != testAllocation() {
			t.Errorf("commit authority identity = user %q allocation %+v", userID, allocation)
		}
		close(denied)
		return nil, false
	}
	candidateHandler := NewTerminalHandler(hub, validator, &stubTerminalOpener{terminal: candidateTerminal}, sessionTarget(true), denyCommit, func() bool { return true }, context.Background(), testFrontendURL)
	candidateRouter := gin.New()
	candidateRouter.GET("/ws/terminal", candidateHandler.HandleWebSocket)
	candidateServer := httptest.NewServer(candidateRouter)
	defer candidateServer.Close()
	candidate, _, err := websocket.DefaultDialer.Dial(wsURL(candidateServer), cookieHeader("good-token"))
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Close()
	attached, err := readOne(candidate, 2*time.Second)
	if err != nil || attached.Type != MsgTerminalAttached || attached.AttachNonce == "" {
		t.Fatalf("candidate attach=%+v err=%v", attached, err)
	}
	if err := candidate.WriteJSON(Message{Type: MsgTerminalReady, AttachNonce: attached.AttachNonce}); err != nil {
		t.Fatal(err)
	}
	if message, err := readOne(candidate, 2*time.Second); err != nil || message.Type != MsgError || message.Message != "terminal session authority changed" {
		t.Fatalf("denied commit response=%+v err=%v", message, err)
	}
	select {
	case <-denied:
	case <-time.After(time.Second):
		t.Fatal("terminal commit authorizer was not called")
	}
	select {
	case <-candidateTerminal.closed:
	case <-time.After(time.Second):
		t.Fatal("denied candidate retained its Runner terminal lease")
	}
	if current := hub.GetClient("u1"); current != existing {
		t.Fatal("denied terminal commit replaced the existing Hub owner")
	}
	go func() { _, _ = firstTerminal.outW.Write([]byte("existing owner remains")) }()
	if output, err := readOne(first, 2*time.Second); err != nil || output.Type != MsgOutput || output.Data != "existing owner remains" {
		t.Fatalf("existing output after commit denial=%+v err=%v", output, err)
	}
}

func TestTerminalExpiredCommitPreservesExistingConnection(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	validator := &stubValidator{users: map[string]*models.User{"good-token": {ID: "u1"}}}
	firstTerminal := newFakeTerminalSession()
	firstHandler := NewTerminalHandler(hub, validator, &stubTerminalOpener{terminal: firstTerminal}, sessionTarget(true), allowTerminalCommit, func() bool { return true }, context.Background(), testFrontendURL)
	gin.SetMode(gin.TestMode)
	firstRouter := gin.New()
	firstRouter.GET("/ws/terminal", firstHandler.HandleWebSocket)
	firstServer := httptest.NewServer(firstRouter)
	defer firstServer.Close()

	first, _, err := websocket.DefaultDialer.Dial(wsURL(firstServer), cookieHeader("good-token"))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := attachTerminal(first, time.Second); err != nil {
		t.Fatal(err)
	}
	existing := waitForHubOwner(t, hub, "u1", func(client *Client) bool { return client != nil })

	candidateTerminal := newFakeTerminalSession()
	expiredCommit := func(ctx context.Context, _ string, _ runner.AllocationRef) (func(), bool) {
		if ctx.Err() != nil {
			return nil, false
		}
		return nil, false
	}
	candidateHandler := NewTerminalHandler(hub, validator, &stubTerminalOpener{terminal: candidateTerminal}, sessionTarget(true), expiredCommit, func() bool { return true }, context.Background(), testFrontendURL)
	candidateRouter := gin.New()
	candidateRouter.GET("/ws/terminal", candidateHandler.HandleWebSocket)
	candidateServer := httptest.NewServer(candidateRouter)
	defer candidateServer.Close()
	candidate, _, err := websocket.DefaultDialer.Dial(wsURL(candidateServer), cookieHeader("good-token"))
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Close()
	attached, err := readOne(candidate, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := candidate.WriteJSON(Message{Type: MsgTerminalReady, AttachNonce: attached.AttachNonce}); err != nil {
		t.Fatal(err)
	}
	if message, err := readOne(candidate, time.Second); err != nil || message.Type != MsgError {
		t.Fatalf("expired candidate response=%+v err=%v", message, err)
	}
	select {
	case <-candidateTerminal.closed:
	case <-time.After(time.Second):
		t.Fatal("expired candidate retained Runner terminal lease")
	}
	if current := hub.GetClient("u1"); current != existing {
		t.Fatal("expired candidate replaced the existing owner")
	}
}

func TestTerminalDisconnectAfterAckBeforeReadyPreservesExistingConnection(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	firstTerminal := newFakeTerminalSession()
	secondTerminal := newFakeTerminalSession()
	opener := &sequencedTerminalOpener{terminals: []runner.TerminalSession{firstTerminal, secondTerminal}}
	validator := &stubValidator{users: map[string]*models.User{"good-token": {ID: "u1"}}}
	handler := NewTerminalHandler(hub, validator, opener, sessionTarget(true), allowTerminalCommit, func() bool { return true }, context.Background(), testFrontendURL)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/ws/terminal", handler.HandleWebSocket)
	server := httptest.NewServer(router)
	defer server.Close()

	first, _, err := websocket.DefaultDialer.Dial(wsURL(server), cookieHeader("good-token"))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := attachTerminal(first, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	existing := waitForHubOwner(t, hub, "u1", func(client *Client) bool { return client != nil })

	candidate, _, err := websocket.DefaultDialer.Dial(wsURL(server), cookieHeader("good-token"))
	if err != nil {
		t.Fatal(err)
	}
	attached, err := readOne(candidate, 2*time.Second)
	if err != nil || attached.Type != MsgTerminalAttached || attached.AttachNonce == "" {
		t.Fatalf("candidate ACK=%+v err=%v", attached, err)
	}
	if current := hub.GetClient("u1"); current != existing {
		t.Fatal("ACK without terminal_ready replaced the established terminal")
	}
	_ = candidate.Close()
	select {
	case <-secondTerminal.closed:
	case <-time.After(time.Second):
		t.Fatal("disconnected pre-ready candidate retained its Runner lease")
	}
	if current := hub.GetClient("u1"); current != existing {
		t.Fatal("disconnected pre-ready candidate changed Hub ownership")
	}
	go func() { _, _ = firstTerminal.outW.Write([]byte("still established")) }()
	if output, err := readOne(first, 2*time.Second); err != nil || output.Type != MsgOutput || output.Data != "still established" {
		t.Fatalf("established output after candidate disconnect=%+v err=%v", output, err)
	}
}

func TestTerminalDisconnectAfterReadyBeforeHubCASPreservesExistingConnection(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	firstTerminal := newFakeTerminalSession()
	secondTerminal := newFakeTerminalSession()
	opener := &sequencedTerminalOpener{terminals: []runner.TerminalSession{firstTerminal, secondTerminal}}
	validator := &stubValidator{users: map[string]*models.User{"good-token": {ID: "u1"}}}
	handler := NewTerminalHandler(hub, validator, opener, sessionTarget(true), allowTerminalCommit, func() bool { return true }, context.Background(), testFrontendURL)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/ws/terminal", handler.HandleWebSocket)
	server := httptest.NewServer(router)
	defer server.Close()

	first, _, err := websocket.DefaultDialer.Dial(wsURL(server), cookieHeader("good-token"))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := attachTerminal(first, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	existing := waitForHubOwner(t, hub, "u1", func(client *Client) bool { return client != nil })

	beforeReplace := make(chan struct{})
	contextCancelled := make(chan struct{})
	releaseReplace := make(chan struct{})
	handler.beforeReplace = func(ctx context.Context) {
		close(beforeReplace)
		<-ctx.Done()
		close(contextCancelled)
		<-releaseReplace
	}
	candidate, _, err := websocket.DefaultDialer.Dial(wsURL(server), cookieHeader("good-token"))
	if err != nil {
		t.Fatal(err)
	}
	attached, err := readOne(candidate, 2*time.Second)
	if err != nil || attached.Type != MsgTerminalAttached || attached.AttachNonce == "" {
		t.Fatalf("candidate ACK=%+v err=%v", attached, err)
	}
	if err := candidate.WriteJSON(Message{Type: MsgTerminalReady, AttachNonce: attached.AttachNonce}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-beforeReplace:
	case <-time.After(time.Second):
		t.Fatal("server did not consume exact terminal_ready before commit hook")
	}
	_ = candidate.Close()
	select {
	case <-contextCancelled:
	case <-time.After(time.Second):
		t.Fatal("candidate disconnect did not cancel precommit context")
	}
	if current := hub.GetClient("u1"); current != existing {
		t.Fatal("candidate changed Hub ownership while commit was paused")
	}
	close(releaseReplace)
	select {
	case <-secondTerminal.closed:
	case <-time.After(time.Second):
		t.Fatal("cancelled post-ready candidate retained its Runner lease")
	}
	if current := hub.GetClient("u1"); current != existing {
		t.Fatal("cancelled post-ready candidate evicted established owner")
	}
	go func() { _, _ = firstTerminal.outW.Write([]byte("old owner preserved")) }()
	if output, err := readOne(first, 2*time.Second); err != nil || output.Type != MsgOutput || output.Data != "old owner preserved" {
		t.Fatalf("established output after precommit cancellation=%+v err=%v", output, err)
	}
}

func TestTerminalReplacementRejectsReplayedAttachNonce(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	firstTerminal := newFakeTerminalSession()
	secondTerminal := newFakeTerminalSession()
	opener := &sequencedTerminalOpener{terminals: []runner.TerminalSession{firstTerminal, secondTerminal}}
	validator := &stubValidator{users: map[string]*models.User{"good-token": {ID: "u1"}}}
	handler := NewTerminalHandler(hub, validator, opener, sessionTarget(true), allowTerminalCommit, func() bool { return true }, context.Background(), testFrontendURL)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/ws/terminal", handler.HandleWebSocket)
	server := httptest.NewServer(router)
	defer server.Close()

	first, _, err := websocket.DefaultDialer.Dial(wsURL(server), cookieHeader("good-token"))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	firstAttached, err := attachTerminal(first, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	firstOwner := waitForHubOwner(t, hub, "u1", func(client *Client) bool { return client != nil })

	second, _, err := websocket.DefaultDialer.Dial(wsURL(server), cookieHeader("good-token"))
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	secondAttached, err := readOne(second, 2*time.Second)
	if err != nil || secondAttached.Type != MsgTerminalAttached || secondAttached.AttachNonce == "" {
		t.Fatalf("replacement ACK=%+v err=%v", secondAttached, err)
	}
	if secondAttached.AttachNonce == firstAttached.AttachNonce {
		t.Fatal("independent terminal attaches reused a nonce")
	}
	if err := second.WriteJSON(Message{Type: MsgTerminalReady, AttachNonce: firstAttached.AttachNonce}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if current := hub.GetClient("u1"); current != firstOwner {
		t.Fatal("replayed prior attach nonce committed replacement")
	}
	if err := second.WriteJSON(Message{Type: MsgTerminalReady, AttachNonce: secondAttached.AttachNonce}); err != nil {
		t.Fatal(err)
	}
	waitForHubOwner(t, hub, "u1", func(owner *Client) bool { return owner != nil && owner != firstOwner })
}

func TestTerminalAccessTokenExpiryClosesBrowserAndLegacySockets(t *testing.T) {
	for _, browser := range []bool{true, false} {
		name := "legacy"
		if browser {
			name = "browser_cookie"
		}
		t.Run(name, func(t *testing.T) {
			hub := NewHub()
			go hub.Run()
			terminal := newFakeTerminalSession()
			validator := &stubValidator{
				users:     map[string]*models.User{"short-token": {ID: "u1"}},
				expiresAt: map[string]time.Time{"short-token": time.Now().Add(250 * time.Millisecond)},
			}
			handler := NewTerminalHandler(hub, validator, &stubTerminalOpener{terminal: terminal}, sessionTarget(true), allowTerminalCommit, func() bool { return true }, context.Background(), testFrontendURL)
			gin.SetMode(gin.TestMode)
			router := gin.New()
			router.GET("/ws/terminal", handler.HandleWebSocket)
			server := httptest.NewServer(router)
			defer server.Close()

			var headers http.Header
			if browser {
				headers = cookieHeader("short-token")
				headers.Set("Origin", testFrontendURL)
			}
			conn, _, err := websocket.DefaultDialer.Dial(wsURL(server), headers)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if !browser {
				if err := conn.WriteJSON(Message{Type: MsgAuth, Token: "short-token"}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := attachTerminal(conn, time.Second); err != nil {
				t.Fatal(err)
			}
			_ = conn.SetReadDeadline(time.Now().Add(time.Second))
			if _, _, err := conn.ReadMessage(); err == nil {
				t.Fatal("terminal socket outlived verified access-token expiry")
			}
			select {
			case <-terminal.closed:
			case <-time.After(time.Second):
				t.Fatal("access-token expiry did not close Runner terminal lease")
			}
		})
	}
}

func TestTerminalSuccessfulReplacementCommitsAfterOpen(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	firstTerminal := newFakeTerminalSession()
	secondTerminal := newFakeTerminalSession()
	opener := &sequencedTerminalOpener{terminals: []runner.TerminalSession{firstTerminal, secondTerminal}}
	validator := &stubValidator{users: map[string]*models.User{"good-token": {ID: "u1"}}}
	handler := NewTerminalHandler(hub, validator, opener, sessionTarget(true), allowTerminalCommit, func() bool { return true }, context.Background(), testFrontendURL)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/ws/terminal", handler.HandleWebSocket)
	srv := httptest.NewServer(r)
	defer srv.Close()

	first, _, err := websocket.DefaultDialer.Dial(wsURL(srv), cookieHeader("good-token"))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if attached, err := attachTerminal(first, 2*time.Second); err != nil || attached.Type != MsgTerminalAttached {
		t.Fatalf("first terminal attach=%+v err=%v", attached, err)
	}
	firstOwner := waitForHubOwner(t, hub, "u1", func(client *Client) bool { return client != nil })

	second, _, err := websocket.DefaultDialer.Dial(wsURL(srv), cookieHeader("good-token"))
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if attached, err := attachTerminal(second, 2*time.Second); err != nil || attached.Type != MsgTerminalAttached {
		t.Fatalf("replacement terminal attach=%+v err=%v", attached, err)
	}
	waitForHubOwner(t, hub, "u1", func(owner *Client) bool { return owner != nil && owner != firstOwner })

	first.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := first.ReadMessage(); err == nil {
		t.Fatal("old browser remained connected after committed replacement")
	} else if closeErr, ok := err.(*websocket.CloseError); !ok || closeErr.Code != CloseConnectionReplaced {
		t.Fatalf("old browser close=%v, want %d", err, CloseConnectionReplaced)
	}
	select {
	case <-firstTerminal.closed:
	case <-time.After(time.Second):
		t.Fatal("committed replacement did not release the prior Runner lease")
	}

	go secondTerminal.outW.Write([]byte("replacement ready"))
	if message, err := readOne(second, 2*time.Second); err != nil || message.Type != MsgOutput || message.Data != "replacement ready" {
		t.Fatalf("replacement terminal output=%+v err=%v", message, err)
	}
}

func TestTerminalSlowDisplacedCloseRunsAfterCommitLeaseRelease(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	firstTerminal := newMutationCountingTerminal()
	secondTerminal := newFakeTerminalSession()
	opener := &sequencedTerminalOpener{terminals: []runner.TerminalSession{firstTerminal, secondTerminal}}
	validator := &stubValidator{users: map[string]*models.User{"good-token": {ID: "u1"}}}
	releaseObserved := make(chan struct{}, 2)
	authorize := func(context.Context, string, runner.AllocationRef) (func(), bool) {
		var once sync.Once
		return func() { once.Do(func() { releaseObserved <- struct{}{} }) }, true
	}
	handler := NewTerminalHandler(hub, validator, opener, sessionTarget(true), authorize, func() bool { return true }, context.Background(), testFrontendURL)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/ws/terminal", handler.HandleWebSocket)
	server := httptest.NewServer(router)
	defer server.Close()

	first, _, err := websocket.DefaultDialer.Dial(wsURL(server), cookieHeader("good-token"))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	firstAttached, err := attachTerminal(first, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	<-releaseObserved
	firstOwner := waitForHubOwner(t, hub, "u1", func(client *Client) bool { return client != nil })
	closeStarted := make(chan struct{})
	closeRelease := make(chan struct{})
	var closeReleaseOnce sync.Once
	releaseClose := func() { closeReleaseOnce.Do(func() { close(closeRelease) }) }
	defer releaseClose()
	firstOwner.beforeClose = func() {
		close(closeStarted)
		<-closeRelease
	}

	second, _, err := websocket.DefaultDialer.Dial(wsURL(server), cookieHeader("good-token"))
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	attached, err := readOne(second, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.WriteJSON(Message{Type: MsgTerminalReady, AttachNonce: attached.AttachNonce}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closeStarted:
	case <-time.After(time.Second):
		t.Fatal("displaced close did not start")
	}
	select {
	case <-releaseObserved:
		// The commit lease is released even though displaced CloseWith remains blocked.
	default:
		t.Fatal("slow displaced close retained terminal commit lease")
	}
	if current := hub.GetClient("u1"); current == nil || current == firstOwner {
		t.Fatal("slow displaced close prevented atomic owner publication")
	}
	select {
	case <-firstTerminal.closed:
	case <-time.After(time.Second):
		t.Fatal("displaced owner retained its Runner lease while close control I/O stalled")
	}
	if err := first.WriteJSON(Message{Type: MsgInput, AttachNonce: firstAttached.AttachNonce, Data: "whoami\n"}); err == nil {
		_ = first.WriteJSON(Message{Type: MsgResize, AttachNonce: firstAttached.AttachNonce, Cols: 120, Rows: 40})
		time.Sleep(20 * time.Millisecond)
	}
	writes, resizes := firstTerminal.mutations()
	if writes != 0 || resizes != 0 {
		t.Fatalf("displaced owner mutated Runner terminal while close stalled: writes=%d resizes=%d", writes, resizes)
	}
	releaseClose()
	go func() { _, _ = secondTerminal.outW.Write([]byte("replacement active")) }()
	if message, err := readOne(second, time.Second); err != nil || message.Type != MsgOutput || message.Data != "replacement active" {
		t.Fatalf("replacement output=%+v err=%v", message, err)
	}
}

func TestTerminalInitialIdleDeadlineClosesWithoutFirstPong(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	terminal := newMutationCountingTerminal()
	validator := &stubValidator{users: map[string]*models.User{"good-token": {ID: "u1"}}}
	handler := NewTerminalHandler(
		hub, validator, &stubTerminalOpener{terminal: terminal}, sessionTarget(true),
		allowTerminalCommit, func() bool { return true }, context.Background(), testFrontendURL,
	)
	handler.idleTimeout = 50 * time.Millisecond
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/ws/terminal", handler.HandleWebSocket)
	server := httptest.NewServer(router)
	defer server.Close()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL(server), cookieHeader("good-token"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := attachTerminal(conn, time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case <-terminal.closed:
	case <-time.After(time.Second):
		t.Fatal("terminal without an initial pong outlived its idle deadline")
	}
}

func TestTerminalLegacyAuthBadToken(t *testing.T) {
	srv, _ := setupTerminalServer(t, sessionTarget(true), newFakeTerminalSession())
	conn, _, err := dialTerminal(t, srv, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	conn.WriteJSON(Message{Type: MsgAuth, Token: "bad-token"})

	m, err := readOne(conn, 2*time.Second)
	if err != nil {
		t.Fatalf("expected error message, got %v", err)
	}
	if m.Type != MsgError || m.Message != "invalid token" {
		t.Errorf("expected invalid token error, got %+v", m)
	}
}

// SEC3-7 unit coverage.
func TestClampResize(t *testing.T) {
	cases := []struct{ cols, rows, wantC, wantR int }{
		{80, 24, 80, 24},
		{0, 0, 1, 1},
		{-5, -1, 1, 1},
		{9999, 5000, 500, 200},
		{500, 200, 500, 200},
		{501, 201, 500, 200},
	}
	for _, tc := range cases {
		c, r := clampResize(tc.cols, tc.rows)
		if c != tc.wantC || r != tc.wantR {
			t.Errorf("clampResize(%d,%d) = (%d,%d), want (%d,%d)", tc.cols, tc.rows, c, r, tc.wantC, tc.wantR)
		}
	}
}
