package ws

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/k8s-quiz/backend/internal/runner"
	"github.com/k8s-quiz/backend/pkg/middleware"
)

const (
	lifecycleSchema         = "k8s-quiz.lifecycle/v1"
	lifecycleSubscribeGrace = 250 * time.Millisecond
	lifecyclePollInterval   = 500 * time.Millisecond
	lifecyclePingInterval   = 30 * time.Second
	lifecycleReplayLimit    = 256
	maxLifecycleMessage     = 4 * 1024
)

// LifecycleReader is deliberately user-scoped. A browser cursor is an
// untrusted lookup candidate; the implementation must re-check ownership on
// every bootstrap and tail read.
type LifecycleReader interface {
	BootstrapLifecycle(context.Context, string, *runner.LifecycleCursor) (runner.LifecycleBootstrap, error)
	ReadLifecycleDelta(context.Context, string, runner.LifecycleCursor, int) (runner.LifecycleDelta, error)
}

type LifecycleHandler struct {
	reader        LifecycleReader
	validator     ExpiringTokenValidator
	shutdownCtx   context.Context
	allowedOrigin *url.URL
	upgrader      websocket.Upgrader

	mu          sync.Mutex
	connections map[string]*lifecycleConnection

	subscribeGrace time.Duration
	pollInterval   time.Duration
	pingInterval   time.Duration
}

type lifecycleConnection struct {
	conn   *websocket.Conn
	cancel context.CancelFunc
}

type lifecycleSubscribe struct {
	Type   string                  `json:"type"`
	Cursor *runner.LifecycleCursor `json:"cursor"`
}

type lifecycleReadResult struct {
	data []byte
	err  error
}

type lifecycleSessionFrame struct {
	SessionID          string                        `json:"session_id"`
	ProblemID          string                        `json:"problem_id"`
	Generation         uint64                        `json:"generation"`
	OperationID        string                        `json:"operation_id"`
	Status             string                        `json:"status"`
	TimeoutAt          *time.Time                    `json:"timeout_at"`
	CleanupPending     bool                          `json:"cleanup_pending"`
	TerminalReason     *string                       `json:"terminal_reason"`
	LatestVerifyResult *runner.LifecycleVerifyResult `json:"latest_verify_result"`
}

type lifecycleSnapshotFrame struct {
	Type    string                  `json:"type"`
	Schema  string                  `json:"schema"`
	Session *lifecycleSessionFrame  `json:"session"`
	Cursor  *runner.LifecycleCursor `json:"cursor"`
}

type lifecycleEventFrame struct {
	Type          string          `json:"type"`
	Schema        string          `json:"schema"`
	SessionID     string          `json:"session_id"`
	Generation    uint64          `json:"generation"`
	EventSequence uint64          `json:"event_sequence"`
	EventType     string          `json:"event_type"`
	ReasonCode    string          `json:"reason_code"`
	Message       string          `json:"message"`
	OccurredAt    time.Time       `json:"occurred_at"`
	Payload       json.RawMessage `json:"payload"`
}

type lifecycleResyncFrame struct {
	Type   string `json:"type"`
	Reason string `json:"reason"`
}

type lifecycleErrorFrame struct {
	Type  string `json:"type"`
	Error string `json:"error"`
}

func NewLifecycleHandler(reader LifecycleReader, validator ExpiringTokenValidator, shutdownCtx context.Context, frontendURL string) *LifecycleHandler {
	if shutdownCtx == nil {
		shutdownCtx = context.Background()
	}
	h := &LifecycleHandler{
		reader:         reader,
		validator:      validator,
		shutdownCtx:    shutdownCtx,
		connections:    make(map[string]*lifecycleConnection),
		subscribeGrace: lifecycleSubscribeGrace,
		pollInterval:   lifecyclePollInterval,
		pingInterval:   lifecyclePingInterval,
	}
	if u, err := url.Parse(frontendURL); err == nil && u.Scheme != "" && u.Host != "" {
		h.allowedOrigin = u
	}
	h.upgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin:     h.checkOrigin,
	}
	return h
}

// checkOrigin is stricter than the terminal endpoint: lifecycle is a
// cookie-only browser channel, so a missing Origin is rejected too.
func (h *LifecycleHandler) checkOrigin(r *http.Request) bool {
	if h.allowedOrigin == nil {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	o, err := url.Parse(origin)
	if err != nil || o.Scheme == "" || o.Host == "" || o.User != nil || o.RawQuery != "" || o.Fragment != "" || (o.Path != "" && o.Path != "/") {
		return false
	}
	return strings.EqualFold(o.Scheme, h.allowedOrigin.Scheme) &&
		strings.EqualFold(o.Host, h.allowedOrigin.Host)
}

func (h *LifecycleHandler) HandleWebSocket(c *gin.Context) {
	if h.reader == nil || h.validator == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "lifecycle service unavailable"})
		return
	}
	if !h.checkOrigin(c.Request) {
		c.JSON(http.StatusForbidden, gin.H{"error": "origin not allowed"})
		return
	}
	token, err := c.Cookie(middleware.AccessTokenCookie)
	if err != nil || token == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}
	user, expiresAt, err := h.validator.ValidateAccessTokenWithExpiry(token)
	if err != nil || user == nil || user.ID == "" || !expiresAt.After(time.Now()) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
		return
	}

	conn, err := h.upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("lifecycle websocket upgrade error: %v", err)
		return
	}
	conn.SetReadLimit(maxLifecycleMessage)

	ctx, cancel := context.WithDeadline(h.shutdownCtx, expiresAt)
	stopExpiryClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	current := &lifecycleConnection{conn: conn, cancel: cancel}
	previous := h.register(user.ID, current)
	if previous != nil {
		_ = h.writeControl(previous.conn, websocket.CloseMessage,
			websocket.FormatCloseMessage(CloseConnectionReplaced, connectionReplacedReason))
		previous.cancel()
		_ = previous.conn.Close()
	}
	defer func() {
		cancel()
		stopExpiryClose()
		h.unregister(user.ID, current)
		_ = conn.Close()
	}()

	readFirst := make(chan lifecycleReadResult, 1)
	readDone := make(chan struct{})
	go lifecycleReadPump(conn, readFirst, readDone)

	resume, ok := h.readOptionalSubscribe(ctx, readFirst, readDone)
	if !ok {
		return
	}
	bootstrap, err := h.reader.BootstrapLifecycle(ctx, user.ID, resume)
	if err != nil {
		if errors.Is(err, runner.ErrLifecycleNotFoundOrForbidden) {
			_ = h.writeJSON(conn, lifecycleErrorFrame{Type: "error", Error: "not_found_or_forbidden"})
		} else if ctx.Err() == nil {
			log.Printf("bootstrap lifecycle for user %s: %v", user.ID, err)
		}
		return
	}
	cursor, err := h.writeBootstrap(conn, resume, bootstrap)
	if err != nil {
		return
	}

	pollTicker := time.NewTicker(h.pollInterval)
	pingTicker := time.NewTicker(h.pingInterval)
	defer pollTicker.Stop()
	defer pingTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-readDone:
			return
		case <-pingTicker.C:
			if err := h.writeControl(conn, websocket.PingMessage, nil); err != nil {
				return
			}
		case <-pollTicker.C:
			if cursor == nil {
				next, readErr := h.reader.BootstrapLifecycle(ctx, user.ID, nil)
				if readErr != nil {
					if ctx.Err() == nil {
						log.Printf("poll lifecycle snapshot for user %s: %v", user.ID, readErr)
					}
					return
				}
				if next.Snapshot == nil {
					continue
				}
				cursor, err = h.writeBootstrap(conn, nil, next)
				if err != nil {
					return
				}
				continue
			}

			delta, readErr := h.reader.ReadLifecycleDelta(ctx, user.ID, *cursor, lifecycleReplayLimit)
			if readErr != nil {
				if errors.Is(readErr, runner.ErrLifecycleCursorUnavailable) {
					cursor, err = h.writeResyncSnapshot(ctx, conn, user.ID, cursor)
					if err != nil {
						return
					}
					continue
				}
				if errors.Is(readErr, runner.ErrLifecycleNotFoundOrForbidden) {
					_ = h.writeJSON(conn, lifecycleErrorFrame{Type: "error", Error: "not_found_or_forbidden"})
				} else if ctx.Err() == nil {
					log.Printf("tail lifecycle for user %s: %v", user.ID, readErr)
				}
				return
			}
			cursor, err = h.writeDelta(ctx, conn, user.ID, cursor, delta)
			if err != nil {
				return
			}
		}
	}
}

func lifecycleReadPump(conn *websocket.Conn, first chan<- lifecycleReadResult, done chan<- struct{}) {
	defer close(done)
	firstPending := true
	for {
		_, data, err := conn.ReadMessage()
		if firstPending {
			firstPending = false
			first <- lifecycleReadResult{data: data, err: err}
		}
		if err != nil {
			return
		}
	}
}

func (h *LifecycleHandler) readOptionalSubscribe(ctx context.Context, first <-chan lifecycleReadResult, readDone <-chan struct{}) (*runner.LifecycleCursor, bool) {
	timer := time.NewTimer(h.subscribeGrace)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, false
	case <-readDone:
		select {
		case result := <-first:
			if result.err == nil {
				return parseLifecycleSubscribe(result.data)
			}
		default:
		}
		return nil, false
	case result := <-first:
		if result.err != nil {
			return nil, false
		}
		return parseLifecycleSubscribe(result.data)
	case <-timer.C:
		return nil, true
	}
}

func parseLifecycleSubscribe(data []byte) (*runner.LifecycleCursor, bool) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var subscribe lifecycleSubscribe
	if err := decoder.Decode(&subscribe); err != nil {
		return nil, false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) || subscribe.Type != "lifecycle_subscribe" {
		return nil, false
	}
	if subscribe.Cursor == nil {
		return nil, true
	}
	if subscribe.Cursor.SessionID == "" || len(subscribe.Cursor.SessionID) > 128 ||
		subscribe.Cursor.Generation == 0 || subscribe.Cursor.EventSequence == 0 {
		return nil, false
	}
	return subscribe.Cursor, true
}

func (h *LifecycleHandler) writeBootstrap(conn *websocket.Conn, resume *runner.LifecycleCursor, bootstrap runner.LifecycleBootstrap) (*runner.LifecycleCursor, error) {
	cursor := lifecycleCursorForSnapshot(bootstrap.Snapshot)
	resync := bootstrap.Resync || !validLifecycleReplay(resume, bootstrap.Snapshot, bootstrap.Events)
	if resync {
		if err := h.writeJSON(conn, lifecycleResyncFrame{Type: "lifecycle_resync_required", Reason: "cursor_unavailable"}); err != nil {
			return nil, err
		}
	} else if err := h.writeEvents(conn, cursor, bootstrap.Events); err != nil {
		return nil, err
	}
	if err := h.writeSnapshot(conn, bootstrap.Snapshot); err != nil {
		return nil, err
	}
	return cursor, nil
}

func (h *LifecycleHandler) writeDelta(ctx context.Context, conn *websocket.Conn, userID string, cursor *runner.LifecycleCursor, delta runner.LifecycleDelta) (*runner.LifecycleCursor, error) {
	if delta.SnapshotChanged {
		if err := h.writeSnapshot(conn, delta.Snapshot); err != nil {
			return nil, err
		}
		return lifecycleCursorForSnapshot(delta.Snapshot), nil
	}
	if len(delta.Events) == 0 {
		return cursor, nil
	}
	if !validLifecycleTail(cursor, delta.Events) {
		return h.writeResyncSnapshot(ctx, conn, userID, cursor)
	}
	if err := h.writeEvents(conn, cursor, delta.Events); err != nil {
		return nil, err
	}
	next := *cursor
	next.EventSequence = delta.Events[len(delta.Events)-1].Sequence
	return &next, nil
}

func (h *LifecycleHandler) writeResyncSnapshot(ctx context.Context, conn *websocket.Conn, userID string, cursor *runner.LifecycleCursor) (*runner.LifecycleCursor, error) {
	fresh, err := h.reader.BootstrapLifecycle(ctx, userID, cursor)
	if err != nil {
		return nil, err
	}
	if err := h.writeJSON(conn, lifecycleResyncFrame{Type: "lifecycle_resync_required", Reason: "cursor_unavailable"}); err != nil {
		return nil, err
	}
	if err := h.writeSnapshot(conn, fresh.Snapshot); err != nil {
		return nil, err
	}
	return lifecycleCursorForSnapshot(fresh.Snapshot), nil
}

func validLifecycleReplay(resume *runner.LifecycleCursor, snapshot *runner.LifecycleSnapshot, events []runner.DurableEvent) bool {
	if resume == nil {
		return len(events) == 0
	}
	if snapshot == nil || resume.SessionID != snapshot.SessionID || resume.Generation != snapshot.Generation ||
		resume.EventSequence > snapshot.EventSequence {
		return false
	}
	if resume.EventSequence == snapshot.EventSequence {
		return len(events) == 0
	}
	expected := resume.EventSequence + 1
	for _, event := range events {
		if event.Sequence != expected || event.Sequence > snapshot.EventSequence {
			return false
		}
		expected++
	}
	return expected == snapshot.EventSequence+1
}

func validLifecycleTail(cursor *runner.LifecycleCursor, events []runner.DurableEvent) bool {
	if cursor == nil {
		return false
	}
	expected := cursor.EventSequence + 1
	for _, event := range events {
		if event.Sequence != expected {
			return false
		}
		expected++
	}
	return true
}

func lifecycleCursorForSnapshot(snapshot *runner.LifecycleSnapshot) *runner.LifecycleCursor {
	if snapshot == nil {
		return nil
	}
	return &runner.LifecycleCursor{
		SessionID:     snapshot.SessionID,
		Generation:    snapshot.Generation,
		EventSequence: snapshot.EventSequence,
	}
}

func (h *LifecycleHandler) writeEvents(conn *websocket.Conn, cursor *runner.LifecycleCursor, events []runner.DurableEvent) error {
	if len(events) == 0 {
		return nil
	}
	if cursor == nil {
		return errors.New("lifecycle events have no session cursor")
	}
	for _, event := range events {
		payload := event.Payload
		if len(payload) == 0 {
			payload = json.RawMessage(`{}`)
		}
		frame := lifecycleEventFrame{
			Type:          "lifecycle_event",
			Schema:        lifecycleSchema,
			SessionID:     cursor.SessionID,
			Generation:    cursor.Generation,
			EventSequence: event.Sequence,
			EventType:     event.Type,
			ReasonCode:    event.ReasonCode,
			Message:       event.Message,
			OccurredAt:    event.CreatedAt,
			Payload:       payload,
		}
		if err := h.writeJSON(conn, frame); err != nil {
			return err
		}
	}
	return nil
}

func (h *LifecycleHandler) writeSnapshot(conn *websocket.Conn, snapshot *runner.LifecycleSnapshot) error {
	frame := lifecycleSnapshotFrame{Type: "lifecycle_snapshot", Schema: lifecycleSchema}
	if snapshot != nil {
		var timeoutAt *time.Time
		if !snapshot.TimeoutAt.IsZero() {
			value := snapshot.TimeoutAt.UTC()
			timeoutAt = &value
		}
		var terminalReason *string
		if snapshot.TerminalReason != "" {
			value := snapshot.TerminalReason
			terminalReason = &value
		}
		frame.Session = &lifecycleSessionFrame{
			SessionID:          snapshot.SessionID,
			ProblemID:          snapshot.ProblemID,
			Generation:         snapshot.Generation,
			OperationID:        snapshot.OperationID,
			Status:             snapshot.Status,
			TimeoutAt:          timeoutAt,
			CleanupPending:     snapshot.CleanupPending,
			TerminalReason:     terminalReason,
			LatestVerifyResult: snapshot.LatestVerifyResult,
		}
		frame.Cursor = lifecycleCursorForSnapshot(snapshot)
	}
	return h.writeJSON(conn, frame)
}

func (h *LifecycleHandler) writeJSON(conn *websocket.Conn, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal lifecycle frame: %w", err)
	}
	if err := conn.SetWriteDeadline(time.Now().Add(wsWriteWait)); err != nil {
		return err
	}
	return conn.WriteMessage(websocket.TextMessage, payload)
}

func (h *LifecycleHandler) writeControl(conn *websocket.Conn, messageType int, payload []byte) error {
	return conn.WriteControl(messageType, payload, time.Now().Add(wsWriteWait))
}

func (h *LifecycleHandler) register(userID string, connection *lifecycleConnection) *lifecycleConnection {
	h.mu.Lock()
	defer h.mu.Unlock()
	previous := h.connections[userID]
	h.connections[userID] = connection
	return previous
}

func (h *LifecycleHandler) unregister(userID string, connection *lifecycleConnection) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.connections[userID] == connection {
		delete(h.connections, userID)
	}
}
