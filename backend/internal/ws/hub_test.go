package ws

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// wsServer starts an httptest server that upgrades connections and registers
// them with the hub under the userID given in the "user" query param.
func wsServer(t *testing.T, hub *Hub) *httptest.Server {
	t.Helper()
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		client := &Client{
			UserID: r.URL.Query().Get("user"),
			Conn:   conn,
			Send:   make(chan []byte, 256),
		}
		if !hub.Register(client) {
			client.Close()
			return
		}
		go client.WritePump()
		// keep the handler alive until the connection dies
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				hub.Unregister(client)
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func dial(t *testing.T, srv *httptest.Server, user string) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/?user=" + user
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func readMsg(t *testing.T, conn *websocket.Conn, timeout time.Duration) Message {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(timeout))
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var m Message
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return m
}

func waitForClient(t *testing.T, hub *Hub, user string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if hub.GetClient(user) != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("client %q not registered in time", user)
}

func TestHubSendToUser(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	srv := wsServer(t, hub)

	conn := dial(t, srv, "u1")
	waitForClient(t, hub, "u1")

	hub.SendToUser("u1", Message{Type: MsgStage, Stage: "k3s_booting", Message: "hi"})
	m := readMsg(t, conn, 2*time.Second)
	if m.Type != MsgStage || m.Stage != "k3s_booting" || m.Message != "hi" {
		t.Fatalf("got %+v, want stage/k3s_booting/hi", m)
	}

	// unknown user: must not panic
	hub.SendToUser("nobody", Message{Type: MsgError})
}

func TestHubReplacesExistingConnection(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	srv := wsServer(t, hub)

	old := dial(t, srv, "u1")
	waitForClient(t, hub, "u1")

	// second connection for the same user replaces the first
	dial(t, srv, "u1")
	deadline := time.Now().Add(2 * time.Second)
	replaced := false
	for time.Now().Before(deadline) && !replaced {
		c := hub.GetClient("u1")
		replaced = c != nil && c.Conn != old
		time.Sleep(10 * time.Millisecond)
	}
	if !replaced {
		t.Fatal("second connection did not replace the first")
	}

	// the old connection must be closed by the hub
	old.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := old.ReadMessage(); err == nil {
		t.Fatal("old connection still readable, want closed")
	} else if closeErr, ok := err.(*websocket.CloseError); !ok || closeErr.Code != CloseConnectionReplaced {
		t.Fatalf("old connection close=%v, want code %d", err, CloseConnectionReplaced)
	}
}

func TestHubUnregister(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	srv := wsServer(t, hub)

	conn := dial(t, srv, "u1")
	waitForClient(t, hub, "u1")

	conn.Close()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if hub.GetClient("u1") == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("client not unregistered after close")
}

func TestClientWriteJSONConcurrent(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	srv := wsServer(t, hub)

	conn := dial(t, srv, "u1")
	waitForClient(t, hub, "u1")

	done := make(chan struct{})
	go func() {
		for i := 0; i < 20; i++ {
			hub.SendToUser("u1", Message{Type: MsgOutput, Data: "x"})
		}
		close(done)
	}()
	go func() {
		for i := 0; i < 20; i++ {
			hub.SendToUser("u1", Message{Type: MsgStage, Stage: "s"})
		}
	}()
	<-done

	for i := 0; i < 40; i++ {
		readMsg(t, conn, 2*time.Second)
	}
}

// WS-4: Broadcast reaches every connected client (server_restart notice on
// graceful shutdown).
func TestHubBroadcast(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	srv := wsServer(t, hub)

	conn1 := dial(t, srv, "u1")
	conn2 := dial(t, srv, "u2")
	waitForClient(t, hub, "u1")
	waitForClient(t, hub, "u2")

	hub.Broadcast(Message{Type: MsgSessionEnded, Reason: "server_restart"})

	for user, conn := range map[string]*websocket.Conn{"u1": conn1, "u2": conn2} {
		m := readMsg(t, conn, 2*time.Second)
		if m.Type != MsgSessionEnded || m.Reason != "server_restart" {
			t.Errorf("%s: expected session_ended/server_restart, got %+v", user, m)
		}
	}
}

func TestHubCloseAllClosesAndRemovesEveryClient(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	srv := wsServer(t, hub)

	conn1 := dial(t, srv, "u1")
	conn2 := dial(t, srv, "u2")
	waitForClient(t, hub, "u1")
	waitForClient(t, hub, "u2")
	hub.CloseAll()
	if hub.GetClient("u1") != nil || hub.GetClient("u2") != nil {
		t.Fatal("CloseAll retained clients")
	}
	for user, conn := range map[string]*websocket.Conn{"u1": conn1, "u2": conn2} {
		conn.SetReadDeadline(time.Now().Add(time.Second))
		if _, _, err := conn.ReadMessage(); err == nil {
			t.Fatalf("%s socket remained open after CloseAll", user)
		}
	}
}

func TestHubCloseAllPermanentlyClosesRegistration(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	hub.CloseAll()
	client := &Client{UserID: "u1", LeaseID: "lease-1"}
	if hub.Register(client) {
		t.Fatal("hub accepted a terminal after CloseAll")
	}
	if hub.GetClient("u1") != nil {
		t.Fatal("rejected terminal remained registered")
	}
}

func TestHubReplaceIfCurrentRejectsLateCandidate(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	first := &Client{UserID: "u1", LeaseID: "lease-1"}
	if !hub.Register(first) {
		t.Fatal("register first client")
	}
	second := &Client{UserID: "u1", LeaseID: "lease-2"}
	if !hub.ReplaceIfCurrent(second, first) {
		t.Fatal("replace exact first client")
	}
	late := &Client{UserID: "u1", LeaseID: "lease-late"}
	if hub.ReplaceIfCurrent(late, first) {
		t.Fatal("late candidate replaced a newer owner")
	}
	if got := hub.GetClient("u1"); got != second {
		t.Fatalf("hub owner = %p, want second %p", got, second)
	}
	hub.CloseAll()
}

func TestHubDetachedReplacementPublishesBeforeDisplacedClose(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	first := &Client{UserID: "u1", LeaseID: "lease-1"}
	if !hub.Register(first) {
		t.Fatal("register first client")
	}
	second := &Client{UserID: "u1", LeaseID: "lease-2"}
	start := time.Now()
	result := hub.replaceIfCurrentContextDetached(context.Background(), second, first)
	if !result.accepted || result.displaced != first {
		t.Fatalf("detached replacement result = %+v", result)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("atomic Hub replacement waited for displaced close: %v", elapsed)
	}
	if got := hub.GetClient("u1"); got != second {
		t.Fatalf("hub owner = %p, want second %p", got, second)
	}
	select {
	case <-first.done:
		t.Fatal("detached replacement closed displaced client before caller released commit fence")
	default:
	}
	closeDisplaced(result.displaced)
	select {
	case <-first.done:
	case <-time.After(time.Second):
		t.Fatal("caller could not close displaced client after detached commit")
	}
	hub.CloseAll()
}

func TestClientFullQueueClosesWithoutBlocking(t *testing.T) {
	client := &Client{UserID: "slow", Send: make(chan []byte, 1)}
	if !client.EnqueueJSON(Message{Type: MsgOutput, Data: "first"}) {
		t.Fatal("first enqueue unexpectedly failed")
	}
	done := make(chan bool, 1)
	go func() {
		done <- client.EnqueueJSON(Message{Type: MsgOutput, Data: "overflow"})
	}()
	select {
	case accepted := <-done:
		if accepted {
			t.Fatal("full queue accepted an overflow message")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("full queue blocked lifecycle delivery")
	}
	select {
	case <-client.done:
	default:
		t.Fatal("full queue did not close the slow client")
	}
}

func TestHubBroadcastDoesNotBlockOnSlowClient(t *testing.T) {
	client := &Client{UserID: "slow", Send: make(chan []byte, 1)}
	client.init()
	client.Send <- []byte("already full")
	hub := NewHub()
	hub.mu.Lock()
	hub.clients[client.UserID] = client
	hub.mu.Unlock()

	done := make(chan struct{})
	go func() {
		hub.Broadcast(Message{Type: MsgSessionEnded, Reason: "server_restart"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("broadcast blocked on a slow client")
	}
	select {
	case <-client.done:
	default:
		t.Fatal("broadcast overflow did not isolate the slow client")
	}
}

// T7: SendToUser delivers only to the addressed user.
func TestHubSendToUserIsolation(t *testing.T) {
	hub := NewHub()
	go hub.Run()
	srv := wsServer(t, hub)

	conn1 := dial(t, srv, "u1")
	conn2 := dial(t, srv, "u2")
	waitForClient(t, hub, "u1")
	waitForClient(t, hub, "u2")

	hub.SendToUser("u1", Message{Type: MsgStage, Stage: "ready", Message: "for u1 only"})

	if m := readMsg(t, conn1, 2*time.Second); m.Type != MsgStage || m.Stage != "ready" {
		t.Errorf("u1: expected stage/ready, got %+v", m)
	}

	conn2.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, _, err := conn2.ReadMessage(); err == nil {
		t.Error("u2 must not receive u1's message")
	}
}
