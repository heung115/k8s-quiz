package session

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/k8s-quiz/backend/pkg/middleware"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	sessions := rg.Group("/sessions")
	sessions.GET("/current", h.GetCurrent)
	sessions.DELETE("/current", h.EndCurrent)
}

func (h *Handler) GetCurrent(c *gin.Context) {
	u := middleware.GetUser(c)
	sess := h.svc.GetSession(u.ID)
	if sess == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "no active session"})
		return
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	c.JSON(http.StatusOK, gin.H{
		"session_id": sess.ID,
		"problem_id": sess.ProblemID,
		"status":     string(sess.Status),
		"timeout_at": sess.TimeoutAt,
	})
}

func (h *Handler) EndCurrent(c *gin.Context) {
	u := middleware.GetUser(c)
	h.svc.EndSession(c.Request.Context(), u.ID)
	c.Status(http.StatusNoContent)
}
