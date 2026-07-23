package problem

import (
	"context"
	"log"
	"net/http"
	"regexp"

	"github.com/gin-gonic/gin"
	"github.com/k8s-quiz/backend/pkg/middleware"
	"github.com/k8s-quiz/backend/pkg/models"
)

// validProblemID matches the directory/URL-safe ids we accept. This blocks
// path-traversal payloads ("../", slashes) at the API boundary as well as in
// the loader.
var validProblemID = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`).MatchString

var (
	validCategory   = map[string]bool{"pod": true, "network": true, "storage": true, "rbac": true, "scheduling": true, "config": true}
	validDifficulty = map[string]bool{"easy": true, "medium": true, "hard": true}
	validType       = map[string]bool{"fix": true, "find": true, "deploy": true}
	validVerify     = map[string]bool{"script": true, "choice": true, "text": true}
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
		log.Printf("start problem %s for %s failed: %v", c.Param("id"), u.ID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start environment"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"session_id": sessionID})
}

func (h *Handler) Reset(c *gin.Context) {
	u := middleware.GetUser(c)
	if err := h.sessionSvc.ResetEnvironment(c.Request.Context(), u.ID); err != nil {
		log.Printf("reset for %s failed: %v", u.ID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to reset environment"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "environment reset initiated"})
}

func (h *Handler) Verify(c *gin.Context) {
	u := middleware.GetUser(c)
	success, verifyLog, err := h.sessionSvc.Verify(c.Request.Context(), u.ID)
	if err != nil {
		log.Printf("verify for %s failed: %v", u.ID, err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "verification failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": success, "log": verifyLog})
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
		c.JSON(http.StatusBadRequest, gin.H{"error": "submission failed"})
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

// validateProblem enforces the id format and enum fields so a stored problem
// can never carry a path-traversal id or unexpected values.
func validateProblem(p *models.Problem) string {
	if !validProblemID(p.ID) {
		return "invalid problem id (use lowercase letters, digits, hyphen)"
	}
	if p.Category != "" && !validCategory[p.Category] {
		return "invalid category"
	}
	if p.Difficulty != "" && !validDifficulty[p.Difficulty] {
		return "invalid difficulty"
	}
	if p.Type != "" && !validType[p.Type] {
		return "invalid type"
	}
	if p.VerifyType != "" && !validVerify[p.VerifyType] {
		return "invalid verify_type"
	}
	return ""
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
	if msg := validateProblem(&p); msg != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": msg})
		return
	}
	if p.TimeoutMinutes == 0 {
		p.TimeoutMinutes = 30
	}
	if err := h.repo.Upsert(c.Request.Context(), &p); err != nil {
		log.Printf("admin create problem %s failed: %v", p.ID, err)
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
	if msg := validateProblem(&p); msg != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": msg})
		return
	}
	if err := h.repo.Upsert(c.Request.Context(), &p); err != nil {
		log.Printf("admin update problem %s failed: %v", p.ID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update problem"})
		return
	}
	c.JSON(http.StatusOK, p)
}

func (h *Handler) Delete(c *gin.Context) {
	if err := h.repo.Delete(c.Request.Context(), c.Param("id")); err != nil {
		log.Printf("admin delete problem %s failed: %v", c.Param("id"), err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete problem"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "deleted"})
}

func (h *Handler) Sync(c *gin.Context) {
	problems, err := h.loader.LoadAll(c.Request.Context())
	if err != nil {
		log.Printf("admin sync failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load problems"})
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
