package ws

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/k8s-quiz/backend/internal/container"
	"github.com/k8s-quiz/backend/pkg/middleware"
)

const maxWSMessage = 64 * 1024 // bound inbound frame size (memory-DoS guard)

type TerminalHandler struct {
	hub           *Hub
	validator     middleware.TokenValidator
	containerMgr  container.Manager
	getSession    func(userID string) string
	allowedOrigin *url.URL
	upgrader      websocket.Upgrader
}

func NewTerminalHandler(hub *Hub, validator middleware.TokenValidator, containerMgr container.Manager, getSession func(string) string, frontendURL string) *TerminalHandler {
	h := &TerminalHandler{
		hub:          hub,
		validator:    validator,
		containerMgr: containerMgr,
		getSession:   getSession,
	}
	if u, err := url.Parse(frontendURL); err == nil {
		h.allowedOrigin = u
	}
	h.upgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin:     h.checkOrigin,
	}
	return h
}

// checkOrigin rejects cross-origin WebSocket upgrades (CSRF / abuse guard).
// An empty Origin (non-browser clients) is permitted; otherwise the origin
// must match the configured frontend.
func (h *TerminalHandler) checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	if h.allowedOrigin == nil {
		return false
	}
	o, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(o.Scheme, h.allowedOrigin.Scheme) &&
		strings.EqualFold(o.Host, h.allowedOrigin.Host)
}

func (h *TerminalHandler) HandleWebSocket(c *gin.Context) {
	conn, err := h.upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("websocket upgrade error: %v", err)
		return
	}
	conn.SetReadLimit(maxWSMessage)

	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	_, msgData, err := conn.ReadMessage()
	if err != nil {
		conn.Close()
		return
	}

	var authMsg Message
	if err := json.Unmarshal(msgData, &authMsg); err != nil || authMsg.Type != MsgAuth || authMsg.Token == "" {
		writeMsg(conn, Message{Type: MsgError, Message: "authentication required"})
		conn.Close()
		return
	}

	u, err := h.validator.ValidateAccessToken(authMsg.Token)
	if err != nil {
		writeMsg(conn, Message{Type: MsgError, Message: "invalid token"})
		conn.Close()
		return
	}

	userID := u.ID
	conn.SetReadDeadline(time.Time{})
	// Keep the connection alive: each pong resets the read deadline so a
	// silently-dead client is reaped instead of leaking a goroutine.
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		return nil
	})

	containerID := h.getSession(userID)
	if containerID == "" {
		writeMsg(conn, Message{Type: MsgError, Message: "no active session"})
		conn.Close()
		return
	}

	client := &Client{
		UserID: userID,
		Conn:   conn,
		Send:   make(chan []byte, 256),
	}
	h.hub.Register(client)

	go h.readPump(client, containerID)
	go h.writePump(client)
}

func (h *TerminalHandler) readPump(client *Client, containerID string) {
	defer func() {
		h.hub.Unregister(client)
		client.Conn.Close()
	}()

	execConn, err := h.containerMgr.ExecInteractive(context.Background(), containerID, []string{"/bin/sh"})
	if err != nil {
		client.WriteJSON(Message{Type: MsgError, Message: "failed to start terminal"})
		log.Printf("terminal exec for %s failed: %v", client.UserID, err)
		return
	}
	defer execConn.Close()

	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := execConn.Read(buf)
			if n > 0 {
				client.WriteJSON(Message{Type: MsgOutput, Data: string(buf[:n])})
			}
			if err != nil {
				if err != io.EOF {
					client.WriteJSON(Message{Type: MsgError, Message: "terminal disconnected"})
				}
				client.Conn.Close()
				return
			}
		}
	}()

	for {
		_, msgData, err := client.Conn.ReadMessage()
		if err != nil {
			return
		}

		var msg Message
		if err := json.Unmarshal(msgData, &msg); err != nil {
			continue
		}

		switch msg.Type {
		case MsgInput:
			execConn.Write([]byte(msg.Data))
		case MsgResize:
			execConn.Resize(msg.Cols, msg.Rows)
		}
	}
}

func (h *TerminalHandler) writePump(client *Client) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			client.mu.Lock()
			err := client.Conn.WriteMessage(websocket.PingMessage, nil)
			client.mu.Unlock()
			if err != nil {
				return
			}
		}
	}
}

func writeMsg(conn *websocket.Conn, msg Message) {
	data, _ := json.Marshal(msg)
	conn.WriteMessage(websocket.TextMessage, data)
}
