package user

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/k8s-quiz/backend/pkg/middleware"
	"github.com/k8s-quiz/backend/pkg/models"
)

type mockUserRepo struct {
	users       []models.User
	leaderboard []models.LeaderboardEntry
	roles       map[string]models.Role
	listErr     error
}

func (m *mockUserRepo) List(ctx context.Context) ([]models.User, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	return m.users, nil
}

func (m *mockUserRepo) UpdateRole(ctx context.Context, id string, role models.Role) error {
	if m.roles == nil {
		m.roles = map[string]models.Role{}
	}
	m.roles[id] = role
	return nil
}

func (m *mockUserRepo) Leaderboard(ctx context.Context, limit int) ([]models.LeaderboardEntry, error) {
	return m.leaderboard, nil
}

type mockProblemRepo struct {
	attempts    []models.Attempt
	allAttempts []models.Attempt
	err         error
}

func (m *mockProblemRepo) ListAttemptsByUser(ctx context.Context, userID string) ([]models.Attempt, error) {
	if m.err != nil {
		return nil, m.err
	}
	var out []models.Attempt
	for _, a := range m.attempts {
		if a.UserID == userID {
			out = append(out, a)
		}
	}
	return out, nil
}

func (m *mockProblemRepo) ListAllAttempts(ctx context.Context) ([]models.Attempt, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.allAttempts, nil
}

var testUser = &models.User{ID: "u1", Username: "alice", Role: models.RoleAdmin}

func setupRouter(ur *mockUserRepo, pr *mockProblemRepo) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewHandler(ur, pr)
	rg := r.Group("/api")
	// inject the authenticated user like middleware.Auth does
	rg.Use(func(c *gin.Context) {
		c.Set(middleware.UserContextKey, testUser)
		c.Next()
	})
	h.RegisterRoutes(rg)
	h.RegisterAdminRoutes(rg)
	return r
}

func do(r *gin.Engine, method, path, body string) *httptest.ResponseRecorder {
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestMyAttemptsEmpty(t *testing.T) {
	r := setupRouter(&mockUserRepo{}, &mockProblemRepo{})
	w := do(r, "GET", "/api/users/me/attempts", "")
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp struct {
		Attempts []models.Attempt `json:"attempts"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Attempts == nil || len(resp.Attempts) != 0 {
		t.Fatalf("attempts = %v, want empty non-nil slice", resp.Attempts)
	}
}

func TestMyAttemptsAndProgress(t *testing.T) {
	dur := 120
	pr := &mockProblemRepo{attempts: []models.Attempt{
		{ID: "a1", UserID: "u1", ProblemID: "p1", Status: "success", DurationSeconds: &dur},
		{ID: "a2", UserID: "u1", ProblemID: "p2", Status: "failed"},
		{ID: "a3", UserID: "other", ProblemID: "p3", Status: "success"},
	}}
	r := setupRouter(&mockUserRepo{}, pr)

	w := do(r, "GET", "/api/users/me/attempts", "")
	var aResp struct {
		Attempts []models.Attempt `json:"attempts"`
	}
	json.Unmarshal(w.Body.Bytes(), &aResp)
	if len(aResp.Attempts) != 2 {
		t.Fatalf("attempts = %d, want 2 (only own)", len(aResp.Attempts))
	}

	w = do(r, "GET", "/api/users/me/progress", "")
	var pResp struct {
		SolvedCount   int             `json:"solved_count"`
		TotalAttempts int             `json:"total_attempts"`
		Solved        map[string]bool `json:"solved"`
	}
	json.Unmarshal(w.Body.Bytes(), &pResp)
	if pResp.SolvedCount != 1 || pResp.TotalAttempts != 2 || !pResp.Solved["p1"] {
		t.Fatalf("progress = %+v, want solved_count=1 total=2 solved[p1]", pResp)
	}
}

func TestMyAchievements(t *testing.T) {
	dur := 120
	pr := &mockProblemRepo{attempts: []models.Attempt{
		{ID: "a1", UserID: "u1", ProblemID: "p1", Status: "success", DurationSeconds: &dur},
	}}
	r := setupRouter(&mockUserRepo{}, pr)
	w := do(r, "GET", "/api/users/me/achievements", "")
	var resp struct {
		Achievements []models.Achievement `json:"achievements"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	byID := map[string]bool{}
	for _, a := range resp.Achievements {
		byID[a.ID] = a.Unlocked
	}
	if !byID["first_solve"] || !byID["speed_solve"] || byID["five_solves"] || byID["persistent"] {
		t.Fatalf("achievements wrong: %+v", byID)
	}
}

func TestLeaderboard(t *testing.T) {
	ur := &mockUserRepo{leaderboard: []models.LeaderboardEntry{
		{Rank: 1, UserID: "u1", Username: "alice", SolvedCount: 3},
	}}
	r := setupRouter(ur, &mockProblemRepo{})
	w := do(r, "GET", "/api/leaderboard", "")
	var resp struct {
		Leaderboard []models.LeaderboardEntry `json:"leaderboard"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Leaderboard) != 1 || resp.Leaderboard[0].Username != "alice" {
		t.Fatalf("leaderboard = %+v", resp)
	}
}

func TestAdminListUsersAndAttempts(t *testing.T) {
	ur := &mockUserRepo{users: []models.User{{ID: "u1", Username: "alice"}}}
	pr := &mockProblemRepo{allAttempts: []models.Attempt{{ID: "a1", UserID: "u1"}}}
	r := setupRouter(ur, pr)

	w := do(r, "GET", "/api/admin/users", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "alice") {
		t.Fatalf("admin users: %d %s", w.Code, w.Body.String())
	}

	w = do(r, "GET", "/api/admin/attempts", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "a1") {
		t.Fatalf("admin attempts: %d %s", w.Code, w.Body.String())
	}
}

func TestUpdateRoleValidation(t *testing.T) {
	ur := &mockUserRepo{}
	r := setupRouter(ur, &mockProblemRepo{})

	w := do(r, "PUT", "/api/admin/users/u1/role", `{"role":"superuser"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid role: status = %d, want 400", w.Code)
	}

	w = do(r, "PUT", "/api/admin/users/u1/role", `{}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing role: status = %d, want 400", w.Code)
	}

	w = do(r, "PUT", "/api/admin/users/u1/role", `{"role":"admin"}`)
	if w.Code != 200 {
		t.Fatalf("valid role: status = %d, want 200", w.Code)
	}
	if ur.roles["u1"] != models.RoleAdmin {
		t.Fatalf("role not updated: %+v", ur.roles)
	}
}

func TestProgressRepoError(t *testing.T) {
	pr := &mockProblemRepo{err: errors.New("db down")}
	r := setupRouter(&mockUserRepo{}, pr)
	w := do(r, "GET", "/api/users/me/progress", "")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
}
