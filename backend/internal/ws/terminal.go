package ws

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/k8s-quiz/backend/internal/runner"
	"github.com/k8s-quiz/backend/pkg/middleware"
	"github.com/k8s-quiz/backend/pkg/models"
)

const (
	maxWSMessage         = 64 * 1024 // bound inbound frame size (memory-DoS guard)
	terminalReadyTimeout = 10 * time.Second
	terminalIdleTimeout  = 90 * time.Second
)

// ExpiringTokenValidator returns only a principal and expiry authenticated by
// the same access token. WebSocket handlers require this stronger contract so
// an established transport cannot outlive its JWT authority.
type ExpiringTokenValidator interface {
	ValidateAccessTokenWithExpiry(string) (*models.User, time.Time, error)
}

type TerminalHandler struct {
	hub                   *Hub
	validator             ExpiringTokenValidator
	terminal              TerminalOpener
	getSession            func(userID string, expected runner.SessionRef) (runner.AllocationRef, bool)
	acquireTerminalCommit TerminalCommitAuthorizer
	ready                 func() bool
	authorityCtx          context.Context
	idleTimeout           time.Duration
	allowedOrigin         *url.URL
	upgrader              websocket.Upgrader
	// beforeReplace is a deterministic test seam at the reversible candidate
	// boundary. Production leaves it nil.
	beforeReplace          func(context.Context)
	beforeTerminalMutation func(context.Context, Message)
}

type TerminalOpener interface {
	OpenTerminal(context.Context, runner.OpenTerminalRequest) (runner.TerminalSession, error)
}

// TerminalCommitAuthorizer acquires the session transition commit lease for an
// exact allocation. A successful caller must release the returned function as
// soon as Hub ownership has been decided.
type TerminalCommitAuthorizer func(context.Context, string, runner.AllocationRef) (release func(), ok bool)

func NewTerminalHandler(hub *Hub, validator ExpiringTokenValidator, terminal TerminalOpener, getSession func(string, runner.SessionRef) (runner.AllocationRef, bool), acquireTerminalCommit TerminalCommitAuthorizer, ready func() bool, authorityCtx context.Context, frontendURL string) *TerminalHandler {
	if acquireTerminalCommit == nil {
		panic("terminal commit authorizer is required")
	}
	if authorityCtx == nil {
		authorityCtx = context.Background()
	}
	h := &TerminalHandler{
		hub:                   hub,
		validator:             validator,
		terminal:              terminal,
		getSession:            getSession,
		acquireTerminalCommit: acquireTerminalCommit,
		ready:                 ready,
		authorityCtx:          authorityCtx,
		idleTimeout:           terminalIdleTimeout,
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
	if h.ready == nil || !h.ready() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "runner controller is not ready"})
		return
	}
	expected, err := terminalSessionRef(c.Request.URL.Query())
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "terminal requires an exact session_id and generation"})
		return
	}
	browserRequest := c.Request.Header.Get("Origin") != ""
	if browserRequest && !h.checkOrigin(c.Request) {
		c.JSON(http.StatusForbidden, gin.H{"error": "origin not allowed"})
		return
	}
	// Primary (browser, FRONT-3): authenticate from the access_token cookie at
	// upgrade time — cookie-authed connections need no auth message.
	var userID string
	var expiresAt time.Time
	if token, err := c.Cookie(middleware.AccessTokenCookie); err == nil && token != "" {
		if u, expiry, err := h.validator.ValidateAccessTokenWithExpiry(token); err == nil && u != nil && expiry.After(time.Now()) {
			userID = u.ID
			expiresAt = expiry
		}
	}
	if browserRequest && userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired browser session"})
		return
	}

	conn, err := h.upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("websocket upgrade error: %v", err)
		return
	}
	conn.SetReadLimit(maxWSMessage)
	stopAuthorityClose := context.AfterFunc(h.authorityCtx, func() { _ = conn.Close() })
	defer stopAuthorityClose()

	if userID == "" {
		// Fallback (non-browser clients): legacy first-message auth
		// {"type":"auth","token":"..."}.
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

		u, expiry, err := h.validator.ValidateAccessTokenWithExpiry(authMsg.Token)
		if err != nil || u == nil || !expiry.After(time.Now()) {
			writeMsg(conn, Message{Type: MsgError, Message: "invalid token"})
			conn.Close()
			return
		}
		userID = u.ID
		expiresAt = expiry
	}

	if err := conn.SetReadDeadline(time.Now().Add(h.idleTimeout)); err != nil {
		_ = conn.Close()
		return
	}
	// Keep the connection alive: each pong resets the read deadline so a
	// silently-dead client is reaped instead of leaking a goroutine.
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(h.idleTimeout))
	})

	allocation, ok := h.getSession(userID, expected)
	if !ok || allocation.ID == "" || !h.ready() {
		writeMsg(conn, Message{Type: MsgError, Message: "session identity is not the current ready terminal target"})
		conn.Close()
		return
	}

	client := &Client{
		UserID: userID,
		Conn:   conn,
		Send:   make(chan []byte, 256),
	}
	if !stopAuthorityClose() && h.authorityCtx.Err() != nil {
		client.Close()
		return
	}
	go h.serveTerminal(client, allocation, expiresAt)
}

func terminalSessionRef(values url.Values) (runner.SessionRef, error) {
	sessionID := values.Get("session_id")
	generation, err := strconv.ParseUint(values.Get("generation"), 10, 64)
	if sessionID == "" || err != nil || generation == 0 {
		return runner.SessionRef{}, errors.New("invalid terminal session identity")
	}
	return runner.SessionRef{SessionID: sessionID, Generation: generation}, nil
}

func (h *TerminalHandler) serveTerminal(client *Client, allocation runner.AllocationRef, expiresAt time.Time) {
	ctx, cancel := context.WithDeadline(h.authorityCtx, expiresAt)
	client.cancel = cancel
	registered := false
	stopContextClose := context.AfterFunc(ctx, client.Close)
	defer func() {
		cancel()
		stopContextClose()
		if registered {
			h.hub.Unregister(client)
		}
		client.Close()
	}()

	messages := make(chan Message, 32)
	go readTerminalSocket(ctx, client.Conn, messages, cancel)

	leaseID, err := newTerminalLeaseID()
	if err != nil {
		_ = client.WriteJSON(Message{Type: MsgError, Message: "failed to start terminal"})
		return
	}
	client.LeaseID = leaseID
	previous := h.hub.GetClient(client.UserID)
	previousLeaseID := ""
	if previous != nil {
		previousLeaseID = previous.LeaseID
	}
	execConn, err := h.terminal.OpenTerminal(ctx, runner.OpenTerminalRequest{
		Allocation:      allocation,
		UserID:          client.UserID,
		LeaseID:         leaseID,
		ReplacesLeaseID: previousLeaseID,
	})
	if err != nil {
		if ctx.Err() == nil {
			_ = client.WriteJSON(Message{Type: MsgError, Message: "failed to start terminal"})
		}
		log.Printf("terminal exec for %s failed: %v", client.UserID, err)
		return
	}
	exec := newSynchronizedTerminal(ctx, execConn)
	defer exec.Close()
	if ctx.Err() != nil {
		return
	}

	// Provider open can be slow. Re-resolve the exact current allocation before
	// exposing a candidate lease or touching the established Hub owner.
	if !h.terminalTargetCurrent(client.UserID, allocation) {
		_ = client.WriteJSON(Message{Type: MsgError, Message: "terminal session authority changed"})
		return
	}
	attachNonce, err := newTerminalNonce()
	if err != nil {
		_ = client.WriteJSON(Message{Type: MsgError, Message: "failed to start terminal"})
		return
	}
	// The candidate is still reversible here: the previous Hub owner remains
	// active until the ACK is written and the browser proves receipt by echoing
	// this unpredictable nonce in terminal_ready.
	if err := client.WriteJSON(Message{
		Type:        MsgTerminalAttached,
		SessionID:   allocation.Session.SessionID,
		Generation:  allocation.Session.Generation,
		AttachNonce: attachNonce,
	}); err != nil {
		return
	}
	if !waitTerminalReady(ctx, messages, attachNonce) {
		return
	}
	if h.beforeReplace != nil {
		h.beforeReplace(ctx)
	}
	if ctx.Err() != nil {
		return
	}
	// Acquire the same per-user transition lock used by reset/end immediately
	// before the irreversible Hub takeover. The authorizer rechecks the exact
	// current ready allocation while holding that lock, closing the snapshot-to-
	// CAS race left by ordinary authority reads.
	releaseCommit, ok := h.acquireTerminalCommit(ctx, client.UserID, allocation)
	if !ok || releaseCommit == nil {
		if releaseCommit != nil {
			releaseCommit()
		}
		if ctx.Err() == nil {
			_ = client.WriteJSON(Message{Type: MsgError, Message: "terminal session authority changed"})
		}
		return
	}
	commitResult := func() registerResult {
		defer releaseCommit()
		if ctx.Err() != nil {
			return registerResult{}
		}
		return h.hub.replaceIfCurrentContextDetached(ctx, client, previous)
	}()
	if !commitResult.accepted {
		_ = client.WriteJSON(Message{Type: MsgError, Message: "terminal connection was superseded"})
		return
	}
	registered = true
	// Network close on the displaced browser can wait up to wsWriteWait. It is
	// deliberately after releaseCommit so end/reset/timeout are never blocked
	// by an old client that stopped reading.
	closeDisplaced(commitResult.displaced)
	go client.WritePump()

	terminalDone := make(chan struct{})
	go func() {
		defer close(terminalDone)
		buf := make([]byte, 4096)
		for {
			n, err := exec.Read(buf)
			if n > 0 && ctx.Err() == nil {
				if writeErr := client.WriteJSON(Message{Type: MsgOutput, Data: string(buf[:n])}); writeErr != nil {
					return
				}
			}
			if err != nil {
				if err != io.EOF && ctx.Err() == nil {
					_ = client.WriteJSON(Message{Type: MsgError, Message: "terminal disconnected"})
				}
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-terminalDone:
			return
		case msg, ok := <-messages:
			if !ok {
				return
			}
			// select may choose a buffered frame alongside ctx.Done. Recheck
			// immediately after receive so cancellation wins deterministically.
			if ctx.Err() != nil {
				return
			}
			if msg.AttachNonce != attachNonce {
				continue
			}
			if h.beforeTerminalMutation != nil {
				h.beforeTerminalMutation(ctx, msg)
			}
			switch msg.Type {
			case MsgInput:
				_, _ = exec.Write([]byte(msg.Data))
			case MsgResize:
				cols, rows := clampResize(msg.Cols, msg.Rows)
				_ = exec.Resize(cols, rows)
			}
		}
	}
}

// synchronizedTerminal serializes Close with provider mutations and checks
// cancellation while holding the same mutex immediately before Write/Resize.
// A frame buffered before cancellation therefore cannot mutate the terminal
// after Close has begun, even though TerminalSession itself has no concurrency
// contract beyond io.ReadWriteCloser.
type synchronizedTerminal struct {
	ctx       context.Context
	session   runner.TerminalSession
	mu        sync.Mutex
	closeOnce sync.Once
}

func newSynchronizedTerminal(ctx context.Context, session runner.TerminalSession) *synchronizedTerminal {
	return &synchronizedTerminal{ctx: ctx, session: session}
}

func (t *synchronizedTerminal) Read(p []byte) (int, error) {
	return t.session.Read(p)
}

func (t *synchronizedTerminal) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.ctx.Err(); err != nil {
		return 0, err
	}
	return t.session.Write(p)
}

func (t *synchronizedTerminal) Resize(cols, rows int) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.ctx.Err(); err != nil {
		return err
	}
	return t.session.Resize(cols, rows)
}

func (t *synchronizedTerminal) Close() error {
	var err error
	t.closeOnce.Do(func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		err = t.session.Close()
	})
	return err
}

func (h *TerminalHandler) terminalTargetCurrent(userID string, allocation runner.AllocationRef) bool {
	if h.getSession == nil || h.ready == nil || !h.ready() {
		return false
	}
	current, ok := h.getSession(userID, allocation.Session)
	return ok && current == allocation && current.ID != "" && h.ready()
}

func waitTerminalReady(ctx context.Context, messages <-chan Message, attachNonce string) bool {
	timer := time.NewTimer(terminalReadyTimeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return false
		case msg, ok := <-messages:
			if !ok {
				return false
			}
			// The nonce is the ordering barrier. Every frame read before the exact
			// ready acknowledgement, including input/resize and stale ready frames,
			// is discarded in receive order.
			if msg.Type == MsgTerminalReady && msg.AttachNonce == attachNonce {
				return true
			}
		}
	}
}

func readTerminalSocket(ctx context.Context, conn *websocket.Conn, messages chan<- Message, cancel context.CancelFunc) {
	defer close(messages)
	defer cancel()
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var msg Message
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		select {
		case messages <- msg:
		case <-ctx.Done():
			return
		default:
			// Bound unauthenticated/pre-ready buffering. A peer that floods the
			// candidate loses only its candidate lease and cannot evict the owner.
			cancel()
			return
		}
	}
}

func newTerminalLeaseID() (string, error) {
	return newTerminalRandom("term-")
}

func newTerminalNonce() (string, error) {
	return newTerminalRandom("attach-")
}

func newTerminalRandom(prefix string) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b), nil
}

// clampResize bounds client-requested PTY dimensions (SEC3-7) so a websocket
// client cannot request absurd sizes.
func clampResize(cols, rows int) (int, int) {
	return clampInt(cols, 1, 500), clampInt(rows, 1, 200)
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func writeMsg(conn *websocket.Conn, msg Message) {
	data, _ := json.Marshal(msg)
	_ = conn.SetWriteDeadline(time.Now().Add(wsWriteWait))
	conn.WriteMessage(websocket.TextMessage, data)
}
