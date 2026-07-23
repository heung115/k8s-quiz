package user

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/k8s-quiz/backend/pkg/middleware"
	"github.com/k8s-quiz/backend/pkg/models"
)

type ProblemRepo interface {
	ListAttemptsByUser(ctx context.Context, userID string) ([]models.Attempt, error)
	ListAllAttempts(ctx context.Context) ([]models.Attempt, error)
}

type UserRepo interface {
	List(ctx context.Context) ([]models.User, error)
	UpdateRole(ctx context.Context, id string, role models.Role) error
	Leaderboard(ctx context.Context, limit int) ([]models.LeaderboardEntry, error)
}

type Handler struct {
	repo        UserRepo
	problemRepo ProblemRepo
}

func NewHandler(repo UserRepo, problemRepo ProblemRepo) *Handler {
	return &Handler{repo: repo, problemRepo: problemRepo}
}

func (h *Handler) RegisterRoutes(rg *gin.RouterGroup) {
	rg.GET("/leaderboard", h.Leaderboard)
	users := rg.Group("/users")
	users.GET("/me/attempts", h.MyAttempts)
	users.GET("/me/progress", h.MyProgress)
	users.GET("/me/achievements", h.MyAchievements)
}

func (h *Handler) RegisterAdminRoutes(rg *gin.RouterGroup) {
	admin := rg.Group("/admin")
	admin.GET("/users", h.ListUsers)
	admin.PUT("/users/:id/role", h.UpdateRole)
	admin.GET("/attempts", h.ListAttempts)
}

func (h *Handler) MyAttempts(c *gin.Context) {
	u := middleware.GetUser(c)
	attempts, err := h.problemRepo.ListAttemptsByUser(c.Request.Context(), u.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list attempts"})
		return
	}
	if attempts == nil {
		attempts = []models.Attempt{}
	}
	c.JSON(http.StatusOK, gin.H{"attempts": attempts})
}

func (h *Handler) MyProgress(c *gin.Context) {
	u := middleware.GetUser(c)
	attempts, err := h.problemRepo.ListAttemptsByUser(c.Request.Context(), u.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get progress"})
		return
	}

	solved := make(map[string]bool)
	totalAttempts := 0
	for _, a := range attempts {
		totalAttempts++
		if a.Status == "success" {
			solved[a.ProblemID] = true
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"solved_count":   len(solved),
		"total_attempts": totalAttempts,
		"solved":         solved,
	})
}

func (h *Handler) ListUsers(c *gin.Context) {
	users, err := h.repo.List(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list users"})
		return
	}
	if users == nil {
		users = []models.User{}
	}
	c.JSON(http.StatusOK, gin.H{"users": users})
}

func (h *Handler) UpdateRole(c *gin.Context) {
	id := c.Param("id")
	var req struct {
		Role string `json:"role" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "role required"})
		return
	}
	if req.Role != "admin" && req.Role != "user" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "role must be admin or user"})
		return
	}
	if err := h.repo.UpdateRole(c.Request.Context(), id, models.Role(req.Role)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update role"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "role updated"})
}

func (h *Handler) ListAttempts(c *gin.Context) {
	attempts, err := h.problemRepo.ListAllAttempts(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list attempts"})
		return
	}
	if attempts == nil {
		attempts = []models.Attempt{}
	}
	c.JSON(http.StatusOK, gin.H{"attempts": attempts})
}

func (h *Handler) Leaderboard(c *gin.Context) {
	entries, err := h.repo.Leaderboard(c.Request.Context(), 50)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get leaderboard"})
		return
	}
	if entries == nil {
		entries = []models.LeaderboardEntry{}
	}
	c.JSON(http.StatusOK, gin.H{"leaderboard": entries})
}

func (h *Handler) MyAchievements(c *gin.Context) {
	u := middleware.GetUser(c)
	attempts, err := h.problemRepo.ListAttemptsByUser(c.Request.Context(), u.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get achievements"})
		return
	}

	total := len(attempts)
	solvedSet := make(map[string]bool)
	hasSpeed := false
	for _, a := range attempts {
		if a.Status == "success" {
			solvedSet[a.ProblemID] = true
			if a.DurationSeconds != nil && *a.DurationSeconds <= 300 {
				hasSpeed = true
			}
		}
	}
	solved := len(solvedSet)

	achievements := []models.Achievement{
		{ID: "first_solve", Title: "첫 해결", Description: "첫 문제를 해결했습니다", Unlocked: solved >= 1},
		{ID: "five_solves", Title: "문제 해결사", Description: "서로 다른 5개의 문제를 해결했습니다", Unlocked: solved >= 5},
		{ID: "speed_solve", Title: "스피드러너", Description: "5분 이내에 문제를 해결했습니다", Unlocked: hasSpeed},
		{ID: "persistent", Title: "끈기", Description: "총 10회 시도했습니다", Unlocked: total >= 10},
		{ID: "dedicated", Title: "헌신", Description: "총 25회 시도했습니다", Unlocked: total >= 25},
	}

	c.JSON(http.StatusOK, gin.H{"achievements": achievements})
}
