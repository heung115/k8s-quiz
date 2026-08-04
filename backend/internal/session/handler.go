package session

import (
	"errors"
	"net/http"
	"regexp"

	"github.com/gin-gonic/gin"
	"github.com/k8s-quiz/backend/internal/runner"
	"github.com/k8s-quiz/backend/pkg/middleware"
)

var endOperationIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,128}$`)

type endSessionRequest struct {
	SessionID  string `json:"session_id" binding:"required"`
	Generation uint64 `json:"generation" binding:"required"`
}

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	sessions := rg.Group("/sessions")
	sessions.GET("/current", h.GetCurrent)
	sessions.POST("/end", h.EndCurrent)
}

func (h *Handler) GetCurrent(c *gin.Context) {
	u := middleware.GetUser(c)
	cur, err := h.svc.GetCurrentSessionSnapshot(c.Request.Context(), u.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load active session"})
		return
	}
	if cur == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "no active session"})
		return
	}
	c.JSON(http.StatusOK, cur)
}

func (h *Handler) EndCurrent(c *gin.Context) {
	u := middleware.GetUser(c)
	operationID := c.GetHeader("Idempotency-Key")
	var req endSessionRequest
	if !endOperationIDPattern.MatchString(operationID) || c.ShouldBindJSON(&req) != nil ||
		len(req.SessionID) > 128 || req.Generation == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "exact session identity and a valid Idempotency-Key are required"})
		return
	}
	decision, err := h.svc.EndSessionOperation(c.Request.Context(), u.ID, runner.SessionRef{
		SessionID: req.SessionID, Generation: req.Generation,
	}, operationID)
	if err != nil {
		if errors.Is(err, ErrServiceStopping) {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "server is shutting down"})
			return
		}
		if errors.Is(err, runner.ErrGenerationStale) || errors.Is(err, runner.ErrIdempotencyConflict) ||
			errors.Is(err, runner.ErrLifecycleConflict) || errors.Is(err, runner.ErrAllocationNotFound) {
			c.JSON(http.StatusConflict, gin.H{"error": "session end precondition no longer matches"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to end session"})
		return
	}
	if decision.State == runner.EndPending {
		c.JSON(http.StatusAccepted, gin.H{
			"request_id": operationID,
			"session_id": decision.Session.SessionID,
			"generation": decision.Session.Generation,
			"status":     "cleanup_pending",
		})
		return
	}
	c.Status(http.StatusNoContent)
}
