package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/k8s-quiz/backend/internal/problem"
	"github.com/k8s-quiz/backend/internal/session"
	"github.com/k8s-quiz/backend/internal/user"
	"github.com/k8s-quiz/backend/pkg/middleware"
	"github.com/k8s-quiz/backend/pkg/models"
)

// T2: admin-route gating through the REAL middleware chain (middleware.Auth +
// middleware.AdminOnly), wired exactly like main() — not handler-local mocks.

// stubValidator resolves signed-token stand-ins to users of a given role.
type stubValidator struct{}

func (stubValidator) ValidateAccessToken(token string) (*models.User, error) {
	switch token {
	case "user-token":
		return &models.User{ID: "u1", Username: "user", Role: models.RoleUser}, nil
	case "admin-token":
		return &models.User{ID: "a1", Username: "admin", Role: models.RoleAdmin}, nil
	}
	return nil, errors.New("invalid token")
}

type mainTestProblemRepo struct{}

func (mainTestProblemRepo) List(ctx context.Context, category, difficulty, ptype string) ([]models.Problem, error) {
	return []models.Problem{{ID: "p1", Title: "One"}}, nil
}
func (mainTestProblemRepo) FindByID(ctx context.Context, id string) (*models.Problem, error) {
	return &models.Problem{ID: id}, nil
}
func (mainTestProblemRepo) Upsert(ctx context.Context, p *models.Problem) error { return nil }
func (mainTestProblemRepo) Delete(ctx context.Context, id string) error         { return nil }

type mainTestLoader struct{}

func (mainTestLoader) LoadAll(ctx context.Context) ([]models.Problem, error) { return nil, nil }

type mainTestSessionSvc struct{}

func (mainTestSessionSvc) StartProblem(ctx context.Context, userID, problemID string) (string, error) {
	return "s1", nil
}
func (mainTestSessionSvc) Verify(ctx context.Context, userID string) (bool, string, error) {
	return false, "", nil
}
func (mainTestSessionSvc) SubmitChoice(ctx context.Context, userID, choiceID string) (bool, error) {
	return false, nil
}
func (mainTestSessionSvc) ResetEnvironment(ctx context.Context, userID string) error { return nil }
func (mainTestSessionSvc) GetCurrentSession(userID string) *session.CurrentSession   { return nil }

type mainTestUserRepo struct{}

func (mainTestUserRepo) List(ctx context.Context) ([]models.User, error) {
	return []models.User{{ID: "u1", Username: "user", Role: models.RoleUser}}, nil
}
func (mainTestUserRepo) FindByID(ctx context.Context, id string) (*models.User, error) {
	return &models.User{ID: id, Username: "user", Role: models.RoleUser}, nil
}
func (mainTestUserRepo) CountAdmins(ctx context.Context) (int, error) { return 2, nil }
func (mainTestUserRepo) UpdateRole(ctx context.Context, id string, role models.Role) error {
	return nil
}
func (mainTestUserRepo) Leaderboard(ctx context.Context, limit int) ([]models.LeaderboardEntry, error) {
	return nil, nil
}

type mainTestAttemptRepo struct{}

func (mainTestAttemptRepo) ListAttemptsByUser(ctx context.Context, userID string) ([]models.Attempt, error) {
	return nil, nil
}
func (mainTestAttemptRepo) ListAllAttempts(ctx context.Context) ([]models.Attempt, error) {
	return nil, nil
}

// newAdminWiringRouter mirrors main()'s protected/admin route wiring with the
// real Auth + AdminOnly middleware.
func newAdminWiringRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api")
	protected := api.Group("")
	protected.Use(middleware.Auth(stubValidator{}))

	problemHandler := problem.NewHandler(mainTestProblemRepo{}, mainTestLoader{}, mainTestSessionSvc{})
	problemHandler.RegisterAdminRoutes(protected.Group("", middleware.AdminOnly()))

	userHandler := user.NewHandler(mainTestUserRepo{}, mainTestAttemptRepo{})
	userHandler.RegisterAdminRoutes(protected.Group("", middleware.AdminOnly()))
	return r
}

func doAdmin(t *testing.T, r *gin.Engine, method, target, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, target, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, target, nil)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestAdminGatingThroughRealMiddleware(t *testing.T) {
	r := newAdminWiringRouter()

	routes := []struct {
		method, target, body string
	}{
		{"GET", "/api/admin/problems", ""},                     // read route
		{"POST", "/api/admin/problems/sync", ""},               // write route (problems)
		{"PUT", "/api/admin/users/u1/role", `{"role":"user"}`}, // write route (users)
		{"GET", "/api/admin/users", ""},
		{"GET", "/api/admin/attempts", ""},
	}

	for _, rt := range routes {
		// No token → 401 (real Auth middleware).
		if w := doAdmin(t, r, rt.method, rt.target, "", rt.body); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without token: expected 401, got %d", rt.method, rt.target, w.Code)
		}
		// role=user → 403 (real AdminOnly middleware).
		if w := doAdmin(t, r, rt.method, rt.target, "user-token", rt.body); w.Code != http.StatusForbidden {
			t.Errorf("%s %s as user: expected 403, got %d", rt.method, rt.target, w.Code)
		}
		// role=admin → 200 (passes the real chain into the handler).
		if w := doAdmin(t, r, rt.method, rt.target, "admin-token", rt.body); w.Code != http.StatusOK {
			t.Errorf("%s %s as admin: expected 200, got %d (%s)", rt.method, rt.target, w.Code, w.Body.String())
		}
	}
}
