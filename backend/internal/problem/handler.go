package problem

import (
	"context"
	"errors"
	"log"
	"net/http"
	"regexp"

	"github.com/gin-gonic/gin"
	"github.com/k8s-quiz/backend/internal/runner"
	"github.com/k8s-quiz/backend/internal/session"
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
	StartProblemOperation(ctx context.Context, userID, problemID, operationID string) (*session.CurrentSession, error)
	Verify(ctx context.Context, userID, operationID string) (bool, string, error)
	SubmitChoice(ctx context.Context, userID, problemID string, expected runner.SessionRef, operationID, choiceID string) (bool, error)
	ResetEnvironmentOperation(ctx context.Context, userID, problemID string, expected runner.SessionRef, operationID string) (*session.CurrentSession, error)
	GetCurrentSession(userID string) *session.CurrentSession
}

type ProblemRepository interface {
	List(ctx context.Context, category, difficulty, ptype string) ([]models.Problem, error)
	ListAll(ctx context.Context) ([]models.Problem, error)
	FindByID(ctx context.Context, id string) (*models.Problem, error)
	FindAnyByID(ctx context.Context, id string) (*models.Problem, error)
	Upsert(ctx context.Context, p *models.Problem) error
	Delete(ctx context.Context, id string) error
}

type ProblemSyncer interface {
	Sync(ctx context.Context) (int, error)
	Healthy() bool
}

type Handler struct {
	repo       ProblemRepository
	syncer     ProblemSyncer
	sessionSvc SessionService
}

func NewHandler(repo ProblemRepository, syncer ProblemSyncer, sessionSvc SessionService) *Handler {
	return &Handler{repo: repo, syncer: syncer, sessionSvc: sessionSvc}
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
	if h.syncer == nil || !h.syncer.Healthy() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "problem catalog is unavailable"})
		return
	}
	problems, err := h.repo.List(c.Request.Context(), c.Query("category"), c.Query("difficulty"), c.Query("type"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list problems"})
		return
	}
	sanitized := make([]models.Problem, len(problems))
	for i, p := range problems {
		sanitized[i] = sanitizeProblem(p)
	}
	c.JSON(http.StatusOK, gin.H{"problems": sanitized})
}

func (h *Handler) Get(c *gin.Context) {
	if h.syncer == nil || !h.syncer.Healthy() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "problem catalog is unavailable"})
		return
	}
	p, err := h.repo.FindByID(c.Request.Context(), c.Param("id"))
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "problem not found"})
		return
	}
	c.JSON(http.StatusOK, sanitizeProblem(*p))
}

// sanitizeProblem returns a copy of p with the answer and grading rubric
// scrubbed, for user-facing endpoints only (admin routes keep the full model).
// Both fields are json:",omitempty", so zeroing drops them from the response;
// choices stay visible because users need the option list.
func sanitizeProblem(p models.Problem) models.Problem {
	p.CatalogActive = false
	p.CorrectChoice = ""
	p.GradingPrompt = ""
	return p
}

func (h *Handler) Start(c *gin.Context) {
	u := middleware.GetUser(c)
	operationID := c.GetHeader("Idempotency-Key")
	if !validOperationID(operationID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "a valid Idempotency-Key header is required"})
		return
	}
	cur, err := h.sessionSvc.StartProblemOperation(c.Request.Context(), u.ID, c.Param("id"), operationID)
	if err != nil {
		switch {
		case errors.Is(err, session.ErrTooManySessions), errors.Is(err, session.ErrTransitionBusy):
			// SESS-3: global concurrency cap reached.
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "server is at capacity; try again later"})
			return
		case errors.Is(err, session.ErrStartCooldown):
			// SESS-3: per-user start throttle.
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "starting too fast; wait a few seconds and retry"})
			return
		case errors.Is(err, session.ErrServiceStopping):
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "server is shutting down"})
			return
		case errors.Is(err, ErrCatalogAdmissionUnavailable):
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "problem catalog is unavailable"})
			return
		case errors.Is(err, runner.ErrIdempotencyConflict), errors.Is(err, runner.ErrActiveSession), errors.Is(err, runner.ErrLifecycleConflict):
			c.JSON(http.StatusConflict, gin.H{"error": "start operation conflicts with existing session state"})
			return
		}
		log.Printf("start problem %s for %s failed: %v", c.Param("id"), u.ID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start environment"})
		return
	}
	// The response is bound to the durable operation that just completed. Do
	// not perform a second mutable "current session" lookup here: a concurrent
	// reset/end must not substitute another generation in this response.
	if cur == nil {
		log.Printf("start problem %s for %s: no active session after start", c.Param("id"), u.ID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start environment"})
		return
	}
	response := *cur
	response.RequestID = operationID
	c.JSON(http.StatusOK, &response)
}

func (h *Handler) Reset(c *gin.Context) {
	u := middleware.GetUser(c)
	operationID := c.GetHeader("Idempotency-Key")
	if !validOperationID(operationID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "a valid Idempotency-Key header is required"})
		return
	}
	var req struct {
		SessionID        string `json:"session_id" binding:"required"`
		SourceGeneration uint64 `json:"source_generation" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || len(req.SessionID) > 128 || req.SourceGeneration == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "session_id and source_generation are required"})
		return
	}
	cur, err := h.sessionSvc.ResetEnvironmentOperation(c.Request.Context(), u.ID, c.Param("id"), runner.SessionRef{
		SessionID: req.SessionID, Generation: req.SourceGeneration,
	}, operationID)
	if err != nil {
		switch {
		case errors.Is(err, session.ErrCleanupPending) && cur != nil:
			response := *cur
			response.RequestID = operationID
			response.CleanupPending = true
			c.JSON(http.StatusAccepted, &response)
			return
		case errors.Is(err, session.ErrStartCooldown), errors.Is(err, session.ErrTransitionBusy):
			// SEC3-5: reset honors the per-user start cooldown.
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "resetting too fast; wait a few seconds and retry"})
			return
		case errors.Is(err, session.ErrServiceStopping):
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "server is shutting down"})
			return
		case errors.Is(err, runner.ErrIdempotencyConflict), errors.Is(err, runner.ErrGenerationStale),
			errors.Is(err, runner.ErrActiveSession), errors.Is(err, runner.ErrLifecycleConflict):
			c.JSON(http.StatusConflict, gin.H{"error": "reset operation conflicts with existing session state"})
			return
		}
		log.Printf("reset for %s failed: %v", u.ID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to reset environment"})
		return
	}
	if cur == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to reset environment"})
		return
	}
	response := *cur
	response.RequestID = operationID
	c.JSON(http.StatusOK, &response)
}

func (h *Handler) Verify(c *gin.Context) {
	u := middleware.GetUser(c)
	operationID := c.GetHeader("Idempotency-Key")
	if !validOperationID(operationID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "a valid Idempotency-Key header is required"})
		return
	}
	success, verifyLog, err := h.sessionSvc.Verify(c.Request.Context(), u.ID, operationID)
	if err != nil {
		if errors.Is(err, session.ErrServiceStopping) {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "server is shutting down"})
			return
		}
		if errors.Is(err, session.ErrVerifyTooFast) {
			// SEC3-4: per-user verify throttle.
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "verifying too fast"})
			return
		}
		if errors.Is(err, session.ErrTransitionBusy) {
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "another session transition is already in progress"})
			return
		}
		if errors.Is(err, session.ErrVerifyInfrastructure) {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "verification service was unavailable; start a new verification"})
			return
		}
		log.Printf("verify for %s failed: %v", u.ID, err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "verification failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": success, "log": verifyLog})
}

var validOperationID = regexp.MustCompile(`^[A-Za-z0-9._:-]{8,128}$`).MatchString

func (h *Handler) SubmitChoice(c *gin.Context) {
	u := middleware.GetUser(c)
	problemID := c.Param("id")
	operationID := c.GetHeader("Idempotency-Key")
	if !validProblemID(problemID) || !validOperationID(operationID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "a valid problem and Idempotency-Key header are required"})
		return
	}
	var req struct {
		SessionID  string `json:"session_id" binding:"required"`
		Generation uint64 `json:"generation" binding:"required"`
		ChoiceID   string `json:"choice_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "session_id, generation, and choice_id are required"})
		return
	}
	if len(req.SessionID) > 128 || len(req.ChoiceID) > 128 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "choice submission exceeds the allowed size"})
		return
	}
	expected := runner.SessionRef{SessionID: req.SessionID, Generation: req.Generation}
	success, err := h.sessionSvc.SubmitChoice(c.Request.Context(), u.ID, problemID, expected, operationID, req.ChoiceID)
	if err != nil {
		switch {
		case errors.Is(err, session.ErrServiceStopping):
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "server is shutting down"})
			return
		case errors.Is(err, session.ErrVerifyTooFast):
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "submitting too fast"})
			return
		case errors.Is(err, session.ErrTransitionBusy):
			c.JSON(http.StatusTooManyRequests, gin.H{"error": "another session transition is already in progress"})
			return
		case errors.Is(err, runner.ErrIdempotencyConflict), errors.Is(err, runner.ErrGenerationStale),
			errors.Is(err, runner.ErrLifecycleConflict):
			c.JSON(http.StatusConflict, gin.H{"error": "choice submission does not match the current session"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "submission failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"request_id": operationID,
		"problem_id": problemID,
		"session_id": expected.SessionID,
		"generation": expected.Generation,
		"success":    success,
	})
}

func (h *Handler) AdminList(c *gin.Context) {
	problems, err := h.repo.ListAll(c.Request.Context())
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
	if existing, err := h.repo.FindAnyByID(c.Request.Context(), p.ID); err == nil && existing.Revision != "" {
		c.JSON(http.StatusConflict, gin.H{"error": "catalog-managed problems must be changed through problem sync"})
		return
	}
	// Runtime revision authority belongs exclusively to the trusted loader.
	// New metadata-only drafts remain unapproved; an existing approved row has
	// already had all revision-owned fields restored above.
	if err := h.repo.Upsert(c.Request.Context(), &p); err != nil {
		if errors.Is(err, ErrCatalogManagedProblem) {
			c.JSON(http.StatusConflict, gin.H{"error": "catalog-managed problems must be changed through problem sync"})
			return
		}
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
	existing, err := h.repo.FindAnyByID(c.Request.Context(), p.ID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "problem not found"})
		return
	}
	if existing.Revision != "" {
		c.JSON(http.StatusConflict, gin.H{"error": "catalog-managed problems must be changed through problem sync"})
		return
	}
	if msg := validateProblem(&p); msg != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": msg})
		return
	}
	if err := h.repo.Upsert(c.Request.Context(), &p); err != nil {
		if errors.Is(err, ErrCatalogManagedProblem) {
			c.JSON(http.StatusConflict, gin.H{"error": "catalog-managed problems must be changed through problem sync"})
			return
		}
		log.Printf("admin update problem %s failed: %v", p.ID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update problem"})
		return
	}
	c.JSON(http.StatusOK, p)
}

func (h *Handler) Delete(c *gin.Context) {
	if err := h.repo.Delete(c.Request.Context(), c.Param("id")); err != nil {
		if errors.Is(err, ErrCatalogManagedProblem) {
			c.JSON(http.StatusConflict, gin.H{"error": "catalog-managed problems must be removed through problem sync"})
			return
		}
		log.Printf("admin delete problem %s failed: %v", c.Param("id"), err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete problem"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "deleted"})
}

func (h *Handler) Sync(c *gin.Context) {
	count, err := h.syncer.Sync(c.Request.Context())
	if err != nil {
		log.Printf("admin sync failed: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to synchronize problems"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"synced": count})
}
