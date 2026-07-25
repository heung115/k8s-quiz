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
	"github.com/k8s-quiz/backend/internal/container"
	"github.com/k8s-quiz/backend/pkg/models"
)

// ---- stubs ----

type stubValidator struct{ users map[string]*models.User }

func (v *stubValidator) ValidateAccessToken(token string) (*models.User, error) {
	u, ok := v.users[token]
	if !ok {
		return nil, errors.New("invalid token")
	}
	return u, nil
}

// fakeTerminalSession pipes terminal output (test→server) and forwarded input
// (server→test) separately so tests can observe both directions.
type fakeTerminalSession struct {
	outR    *io.PipeReader
	outW    *io.PipeWriter
	inR     *io.PipeReader
	inW     *io.PipeWriter
	mu      sync.Mutex
	resizes [][2]int
}

func newFakeTerminalSession() *fakeTerminalSession {
	outR, outW := io.Pipe()
	inR, inW := io.Pipe()
	return &fakeTerminalSession{outR: outR, outW: outW, inR: inR, inW: inW}
}

func (f *fakeTerminalSession) Read(p []byte) (int, error)  { return f.outR.Read(p) }
func (f *fakeTerminalSession) Write(p []byte) (int, error) { return f.inW.Write(p) }
func (f *fakeTerminalSession) Close() error {
	f.outR.Close()
	f.outW.Close()
	f.inR.Close()
	f.inW.Close()
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

type stubTerminalManager struct{ terminal container.TerminalSession }

func (m *stubTerminalManager) Create(ctx context.Context, opts container.CreateOpts) (string, error) {
	return "stub-container", nil
}
func (m *stubTerminalManager) Exec(ctx context.Context, id string, cmd []string) (container.ExecResult, error) {
	return container.ExecResult{ExitCode: 0}, nil
}
func (m *stubTerminalManager) ExecInteractive(ctx context.Context, id string, cmd []string) (container.TerminalSession, error) {
	return m.terminal, nil
}
func (m *stubTerminalManager) Remove(ctx context.Context, id string) error { return nil }
func (m *stubTerminalManager) Logs(ctx context.Context, id string) (string, error) {
	return "", nil
}
func (m *stubTerminalManager) WaitReady(ctx context.Context, id string, check func() bool, timeout time.Duration) error {
	return nil
}
func (m *stubTerminalManager) IsRunning(ctx context.Context, id string) (bool, error) {
	return true, nil
}

const testFrontendURL = "http://terminal.test"

func setupTerminalServer(t *testing.T, getSession func(string) string, term container.TerminalSession) (*httptest.Server, *Hub) {
	t.Helper()
	hub := NewHub()
	go hub.Run()
	validator := &stubValidator{users: map[string]*models.User{
		"good-token": {ID: "u1", Username: "tester", Role: models.RoleUser},
	}}
	h := NewTerminalHandler(hub, validator, &stubTerminalManager{terminal: term}, getSession, testFrontendURL)
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/ws/terminal", h.HandleWebSocket)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, hub
}

func wsURL(srv *httptest.Server) string {
	return "ws" + srv.URL[len("http"):] + "/ws/terminal"
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

// ---- tests ----

// SEC3-7 / checkOrigin: a cross-origin upgrade is rejected with 403.
func TestTerminalWrongOriginRejected(t *testing.T) {
	srv, _ := setupTerminalServer(t, func(string) string { return "c1" }, newFakeTerminalSession())
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

// No cookie + first message is not auth → error + close, no output.
func TestTerminalNoAuthClosed(t *testing.T) {
	srv, _ := setupTerminalServer(t, func(string) string { return "c1" }, newFakeTerminalSession())
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
	srv, _ := setupTerminalServer(t, func(string) string { return "" }, newFakeTerminalSession())
	conn, _, err := dialTerminal(t, srv, cookieHeader("good-token"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	m, err := readOne(conn, 2*time.Second)
	if err != nil {
		t.Fatalf("expected error message, got %v", err)
	}
	if m.Type != MsgError || m.Message != "no active session" {
		t.Errorf("expected no active session error, got %+v", m)
	}
}

// Cookie-authed with a session: no auth message needed; output flows, input
// is forwarded, and resize is clamped (SEC3-7).
func TestTerminalCookieAuthFullDuplex(t *testing.T) {
	fake := newFakeTerminalSession()
	srv, _ := setupTerminalServer(t, func(string) string { return "c1" }, fake)
	conn, _, err := dialTerminal(t, srv, cookieHeader("good-token"))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	// Output injected by the "container" reaches the browser client.
	go fake.outW.Write([]byte("hello from k3s"))
	m, err := readOne(conn, 2*time.Second)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if m.Type != MsgOutput || m.Data != "hello from k3s" {
		t.Errorf("expected output message, got %+v", m)
	}

	// Client input is forwarded to the container.
	conn.WriteJSON(Message{Type: MsgInput, Data: "kubectl get nodes"})
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
	conn.WriteJSON(Message{Type: MsgResize, Cols: 9999, Rows: 0})
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

// Legacy first-message auth (non-browser clients) still works.
func TestTerminalLegacyAuthMessage(t *testing.T) {
	fake := newFakeTerminalSession()
	srv, _ := setupTerminalServer(t, func(string) string { return "c1" }, fake)
	conn, _, err := dialTerminal(t, srv, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	conn.WriteJSON(Message{Type: MsgAuth, Token: "good-token"})
	go fake.outW.Write([]byte("legacy ok"))
	m, err := readOne(conn, 2*time.Second)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if m.Type != MsgOutput || m.Data != "legacy ok" {
		t.Errorf("expected output after legacy auth, got %+v", m)
	}
}

func TestTerminalLegacyAuthBadToken(t *testing.T) {
	srv, _ := setupTerminalServer(t, func(string) string { return "c1" }, newFakeTerminalSession())
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
