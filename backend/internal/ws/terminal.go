package ws

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/k8s-quiz/backend/internal/container"
	"github.com/k8s-quiz/backend/pkg/middleware"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

type TerminalHandler struct {
	hub          *Hub
	validator    middleware.TokenValidator
	containerMgr container.Manager
	getSession   func(userID string) string
}

func NewTerminalHandler(hub *Hub, validator middleware.TokenValidator, containerMgr container.Manager, getSession func(string) string) *TerminalHandler {
	return &TerminalHandler{
		hub:          hub,
		validator:    validator,
		containerMgr: containerMgr,
		getSession:   getSession,
	}
}

func (h *TerminalHandler) HandleWebSocket(c *gin.Context) {
	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("websocket upgrade error: %v", err)
		return
	}

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
		client.WriteJSON(Message{Type: MsgError, Message: "failed to start terminal: " + err.Error()})
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
