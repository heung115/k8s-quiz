package problem

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/k8s-quiz/backend/pkg/middleware"
	"github.com/k8s-quiz/backend/pkg/models"
)

type SessionService interface {
	StartProblem(ctx context.Context, userID, problemID string) (string, error)
	Verify(ctx context.Context, userID string) (bool, string, error)
	SubmitChoice(ctx context.Context, userID, choiceID string) (bool, error)
	ResetEnvironment(ctx context.Context, userID string) error
}

type ProblemRepository interface {
	List(ctx context.Context, category, difficulty, ptype string) ([]models.Problem, error)
	FindByID(ctx context.Context, id string) (*models.Problem, error)
	Upsert(ctx context.Context, p *models.Problem) error
	Delete(ctx context.Context, id string) error
}

type ProblemLoader interface {
	LoadAll(ctx context.Context) ([]models.Problem, error)
}

type Handler struct {
	repo       ProblemRepository
	loader     ProblemLoader
	sessionSvc SessionService
}

func NewHandler(repo ProblemRepository, loader ProblemLoader, sessionSvc SessionService) *Handler {
	return &Handler{repo: repo, loader: loader, sessionSvc: sessionSvc}
}

func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	problems := rg.Group("/problems")
	problems.GET("", h.List)
	problems.GET("/:id", h.Get)
	problems.POST("/:id/start", h.Start)
	problems.POST("/:id/reset", h.Reset)
	problems.POST("/:id/verify", h.Verify)
	problems.POST("/:id/submit", h.SubmitChoice)
}

func (h *Handler) RegisterAdminRoutes(rg *gin.RouterGroup) {
	admin := rg.Group("/admin/problems")
	admin.GET("", h.AdminList)
	admin.POST("", h.Create)
	admin.PUT("/:id", h.Update)
	admin.DELETE("/:id", h.Delete)
	admin.POST("/sync", h.Sync)
}

func (h *Handler) List(c *gin.Context) {
	problems, err := h.repo.List(c.Request.Context(), c.Query("category"), c.Query("difficulty"), c.Query("type"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list problems"})
		return
	}
	if problems == nil {
		problems = []models.Problem{}
	}
	c.JSON(http.StatusOK, gin.H{"problems": problems})
}

func (h *Handler) Get(c *gin.Context) {
	p, err := h.repo.FindByID(c.Request.Context(), c.Param("id"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "problem not found"})
		return
	}
	c.JSON(http.StatusOK, p)
}

func (h *Handler) Start(c *gin.Context) {
	u := middleware.GetUser(c)
	sessionID, err := h.sessionSvc.StartProblem(c.Request.Context(), u.ID, c.Param("id"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"session_id": sessionID})
}

func (h *Handler) Reset(c *gin.Context) {
	u := middleware.GetUser(c)
	if err := h.sessionSvc.ResetEnvironment(c.Request.Context(), u.ID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "environment reset initiated"})
}

func (h *Handler) Verify(c *gin.Context) {
	u := middleware.GetUser(c)
	success, log, err := h.sessionSvc.Verify(c.Request.Context(), u.ID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": success, "log": log})
}

func (h *Handler) SubmitChoice(c *gin.Context) {
	u := middleware.GetUser(c)
	var req struct {
		ChoiceID string `json:"choice_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "choice_id required"})
		return
	}
	success, err := h.sessionSvc.SubmitChoice(c.Request.Context(), u.ID, req.ChoiceID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": success})
}

func (h *Handler) AdminList(c *gin.Context) {
	problems, err := h.repo.List(c.Request.Context(), "", "", "")
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list problems"})
		return
	}
	if problems == nil {
		problems = []models.Problem{}
	}
	c.JSON(http.StatusOK, gin.H{"problems": problems})
}

func (h *Handler) Create(c *gin.Context) {
	var p models.Problem
	if err := c.ShouldBindJSON(&p); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if p.ID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id required"})
		return
	}
	if p.TimeoutMinutes == 0 {
		p.TimeoutMinutes = 30
	}
	if err := h.repo.Upsert(c.Request.Context(), &p); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create problem"})
		return
	}
	c.JSON(http.StatusCreated, p)
}

func (h *Handler) Update(c *gin.Context) {
	var p models.Problem
	if err := c.ShouldBindJSON(&p); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	p.ID = c.Param("id")
	if err := h.repo.Upsert(c.Request.Context(), &p); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update problem"})
		return
	}
	c.JSON(http.StatusOK, p)
}

func (h *Handler) Delete(c *gin.Context) {
	if err := h.repo.Delete(c.Request.Context(), c.Param("id")); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete problem"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "deleted"})
}

func (h *Handler) Sync(c *gin.Context) {
	problems, err := h.loader.LoadAll(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load problems: " + err.Error()})
		return
	}
	count := 0
	for i := range problems {
		if err := h.repo.Upsert(c.Request.Context(), &problems[i]); err == nil {
			count++
		}
	}
	c.JSON(http.StatusOK, gin.H{"synced": count})
}
