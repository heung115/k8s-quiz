package ws

import (
	"encoding/json"
	"sync"

	"github.com/gorilla/websocket"
)

type MessageType string

const (
	MsgAuth          MessageType = "auth"
	MsgInput         MessageType = "input"
	MsgResize        MessageType = "resize"
	MsgOutput        MessageType = "output"
	MsgStage         MessageType = "stage"
	MsgVerifyResult  MessageType = "verify_result"
	MsgTimeoutWarn   MessageType = "timeout_warning"
	MsgSessionEnded  MessageType = "session_ended"
	MsgError         MessageType = "error"
)

type Message struct {
	Type    MessageType `json:"type"`
	Data    string      `json:"data,omitempty"`
	Token   string      `json:"token,omitempty"`
	Cols    int         `json:"cols,omitempty"`
	Rows    int         `json:"rows,omitempty"`
	Stage   string      `json:"stage,omitempty"`
	Message string      `json:"message,omitempty"`
	Success bool        `json:"success,omitempty"`
	Log     string      `json:"log,omitempty"`
	Reason  string      `json:"reason,omitempty"`
	RemainingSeconds int `json:"remaining_seconds,omitempty"`
}

type Client struct {
	UserID string
	Conn   *websocket.Conn
	Send   chan []byte
	mu     sync.Mutex
}

func (c *Client) WriteJSON(msg Message) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.Conn.WriteMessage(websocket.TextMessage, data)
}

type Hub struct {
	clients    map[string]*Client // key: userID
	register   chan *Client
	unregister chan *Client
	mu         sync.RWMutex
}

func NewHub() *Hub {
	return &Hub{
		clients:    make(map[string]*Client),
		register:   make(chan *Client),
		unregister: make(chan *Client),
	}
}

func (h *Hub) Run() {
	for {
		select {
		case client := <-h.register:
			h.mu.Lock()
			if existing, ok := h.clients[client.UserID]; ok {
				existing.Conn.Close()
			}
			h.clients[client.UserID] = client
			h.mu.Unlock()
		case client := <-h.unregister:
			h.mu.Lock()
			if c, ok := h.clients[client.UserID]; ok && c == client {
				delete(h.clients, client.UserID)
			}
			h.mu.Unlock()
		}
	}
}

func (h *Hub) Register(client *Client) {
	h.register <- client
}

func (h *Hub) Unregister(client *Client) {
	h.unregister <- client
}

func (h *Hub) SendToUser(userID string, msg Message) {
	h.mu.RLock()
	client, ok := h.clients[userID]
	h.mu.RUnlock()
	if ok {
		client.WriteJSON(msg)
	}
}

func (h *Hub) GetClient(userID string) *Client {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.clients[userID]
}
