package ws

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const wsWriteWait = 5 * time.Second

const (
	// CloseConnectionReplaced tells an older browser tab that another
	// connection intentionally took ownership. Clients must not reconnect on
	// this code or two tabs can evict each other forever.
	CloseConnectionReplaced  = 4001
	connectionReplacedReason = "connection_replaced"
)

type MessageType string

const (
	MsgAuth             MessageType = "auth"
	MsgInput            MessageType = "input"
	MsgResize           MessageType = "resize"
	MsgOutput           MessageType = "output"
	MsgTerminalAttached MessageType = "terminal_attached"
	MsgTerminalReady    MessageType = "terminal_ready"
	MsgStage            MessageType = "stage"
	MsgVerifyResult     MessageType = "verify_result"
	MsgTimeoutWarn      MessageType = "timeout_warning"
	MsgSessionEnded     MessageType = "session_ended"
	MsgError            MessageType = "error"
)

type Message struct {
	Type             MessageType `json:"type"`
	Data             string      `json:"data,omitempty"`
	Token            string      `json:"token,omitempty"`
	Cols             int         `json:"cols,omitempty"`
	Rows             int         `json:"rows,omitempty"`
	Stage            string      `json:"stage,omitempty"`
	Message          string      `json:"message,omitempty"`
	Success          bool        `json:"success,omitempty"`
	Log              string      `json:"log,omitempty"`
	Reason           string      `json:"reason,omitempty"`
	RemainingSeconds int         `json:"remaining_seconds,omitempty"`
	SessionID        string      `json:"session_id,omitempty"`
	Generation       uint64      `json:"generation,omitempty"`
	AttachNonce      string      `json:"attach_nonce,omitempty"`
}

type Client struct {
	UserID    string
	LeaseID   string
	Conn      *websocket.Conn
	Send      chan []byte
	mu        sync.Mutex
	initOnce  sync.Once
	closeOnce sync.Once
	done      chan struct{}
	cancel    context.CancelFunc
	// beforeClose is a deterministic test seam for a peer whose close control
	// write stalls. Production clients leave it nil.
	beforeClose func()
}

func (c *Client) init() {
	c.initOnce.Do(func() {
		if c.Send == nil {
			c.Send = make(chan []byte, 256)
		}
		c.done = make(chan struct{})
	})
}

// EnqueueJSON is deliberately non-blocking. Lifecycle mutations must never
// wait for a browser that stopped reading its WebSocket.
func (c *Client) EnqueueJSON(msg Message) bool {
	c.init()
	data, err := json.Marshal(msg)
	if err != nil {
		return false
	}
	select {
	case <-c.done:
		return false
	default:
	}
	select {
	case c.Send <- data:
		return true
	default:
		// A full queue means the client cannot keep up. Disconnect it so the
		// queue stays bounded and future callbacks remain non-blocking.
		c.Close()
		return false
	}
}

// WriteJSON writes one control/error frame synchronously. It is used only on
// terminal setup failure paths where the read pump is about to close and an
// asynchronously queued error could otherwise be dropped before WritePump
// flushes it.
func (c *Client) WriteJSON(msg Message) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return c.write(websocket.TextMessage, data)
}

func (c *Client) Close() {
	c.closeWith(0, "")
}

func (c *Client) CloseWith(code int, reason string) {
	c.closeWith(code, reason)
}

func (c *Client) closeWith(code int, reason string) {
	c.init()
	c.closeOnce.Do(func() {
		close(c.done)
		if c.cancel != nil {
			c.cancel()
		}
		if c.beforeClose != nil {
			c.beforeClose()
		}
		if c.Conn != nil {
			if code != 0 {
				c.mu.Lock()
				_ = c.Conn.WriteControl(
					websocket.CloseMessage,
					websocket.FormatCloseMessage(code, reason),
					time.Now().Add(wsWriteWait),
				)
				c.mu.Unlock()
			}
			_ = c.Conn.Close()
		}
	})
}

func (c *Client) write(messageType int, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.Conn.SetWriteDeadline(time.Now().Add(wsWriteWait)); err != nil {
		return err
	}
	return c.Conn.WriteMessage(messageType, data)

}

func (c *Client) WritePump() {
	c.init()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	defer c.Close()
	for {
		select {
		case <-c.done:
			return
		case data := <-c.Send:
			if err := c.write(websocket.TextMessage, data); err != nil {
				return
			}
		case <-ticker.C:
			if err := c.write(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

type Hub struct {
	clients    map[string]*Client // key: userID
	register   chan registerRequest
	unregister chan *Client
	mu         sync.RWMutex
	closed     bool
}

type registerRequest struct {
	client          *Client
	expectedCurrent *Client
	compareCurrent  bool
	ctx             context.Context
	result          chan registerResult
}

type registerResult struct {
	accepted  bool
	displaced *Client
}

func NewHub() *Hub {
	return &Hub{
		clients:    make(map[string]*Client),
		register:   make(chan registerRequest),
		unregister: make(chan *Client),
	}
}

func (h *Hub) Run() {
	for {
		select {
		case request := <-h.register:
			client := request.client
			client.init()
			h.mu.Lock()
			existing := h.clients[client.UserID]
			contextActive := request.ctx == nil || request.ctx.Err() == nil
			accepted := !h.closed && contextActive && (!request.compareCurrent || existing == request.expectedCurrent)
			if accepted {
				h.clients[client.UserID] = client
			}
			h.mu.Unlock()
			var displaced *Client
			if accepted && existing != nil && existing != client {
				displaced = existing
			}
			// Publish the atomic ownership decision without waiting for network I/O
			// on the displaced socket. The requester closes displaced after any
			// surrounding lifecycle commit lease has been released.
			request.result <- registerResult{accepted: accepted, displaced: displaced}
		case client := <-h.unregister:
			h.mu.Lock()
			if c, ok := h.clients[client.UserID]; ok && c == client {
				delete(h.clients, client.UserID)
			}
			h.mu.Unlock()
			client.Close()
		}
	}
}

func (h *Hub) Register(client *Client) bool {
	result := h.registerClient(context.Background(), client, nil, false)
	closeDisplaced(result.displaced)
	return result.accepted
}

// ReplaceIfCurrent atomically commits a prepared terminal only if the Hub
// owner observed before provider open is still current. A late candidate can
// never evict a newer connection.
func (h *Hub) ReplaceIfCurrent(client, expectedCurrent *Client) bool {
	result := h.registerClient(context.Background(), client, expectedCurrent, true)
	closeDisplaced(result.displaced)
	return result.accepted
}

// ReplaceIfCurrentContext is the terminal commit point. A candidate whose
// socket/auth context was cancelled before the Hub serializes this request is
// rejected without evicting the established owner.
func (h *Hub) ReplaceIfCurrentContext(ctx context.Context, client, expectedCurrent *Client) bool {
	result := h.replaceIfCurrentContextDetached(ctx, client, expectedCurrent)
	closeDisplaced(result.displaced)
	return result.accepted
}

// replaceIfCurrentContextDetached atomically changes Hub ownership but leaves
// the displaced connection open. Terminal setup uses this result to release
// its session-transition commit lease before potentially slow close I/O.
func (h *Hub) replaceIfCurrentContextDetached(ctx context.Context, client, expectedCurrent *Client) registerResult {
	if ctx == nil {
		return registerResult{}
	}
	return h.registerClient(ctx, client, expectedCurrent, true)
}

func (h *Hub) registerClient(ctx context.Context, client, expectedCurrent *Client, compareCurrent bool) registerResult {
	result := make(chan registerResult, 1)
	request := registerRequest{
		client:          client,
		expectedCurrent: expectedCurrent,
		compareCurrent:  compareCurrent,
		ctx:             ctx,
		result:          result,
	}
	select {
	case h.register <- request:
	case <-ctx.Done():
		return registerResult{}
	}
	// Once serialized, always observe the Hub's authoritative decision. It may
	// have committed immediately before cancellation; the caller must then mark
	// itself registered so its deferred unregister removes that exact owner.
	return <-result
}

func closeDisplaced(client *Client) {
	if client != nil {
		client.CloseWith(CloseConnectionReplaced, connectionReplacedReason)
	}
}

func (h *Hub) Unregister(client *Client) {
	h.unregister <- client
}

func (h *Hub) SendToUser(userID string, msg Message) {
	h.mu.RLock()
	client, ok := h.clients[userID]
	h.mu.RUnlock()
	if ok {
		client.EnqueueJSON(msg)
	}
}

// Broadcast sends a best-effort message to every connected terminal client.
func (h *Hub) Broadcast(msg Message) {
	h.mu.RLock()
	clients := make([]*Client, 0, len(h.clients))
	for _, client := range h.clients {
		clients = append(clients, client)
	}
	h.mu.RUnlock()
	for _, client := range clients {
		client.EnqueueJSON(msg)
	}
}

// CloseAll synchronously closes every browser socket. Runner terminal leases
// are tied to their read pumps and close as those sockets unwind.
func (h *Hub) CloseAll() {
	h.mu.Lock()
	h.closed = true
	clients := make([]*Client, 0, len(h.clients))
	for userID, client := range h.clients {
		clients = append(clients, client)
		delete(h.clients, userID)
	}
	h.mu.Unlock()
	for _, client := range clients {
		client.Close()
	}
}

func (h *Hub) GetClient(userID string) *Client {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.clients[userID]
}
