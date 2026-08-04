package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/k8s-quiz/backend/internal/problem"
	"github.com/k8s-quiz/backend/internal/runner"
	"github.com/k8s-quiz/backend/internal/session"
	"github.com/k8s-quiz/backend/internal/user"
	"github.com/k8s-quiz/backend/pkg/config"
	"github.com/k8s-quiz/backend/pkg/middleware"
	"github.com/k8s-quiz/backend/pkg/models"
)

func TestRuntimeDatabasePoolConfigPinsIdentityAndSearchPath(t *testing.T) {
	cfg := &config.Config{
		DatabaseURL:              "postgres://kq_cp_runtime:secret@db.example/k8squiz?sslmode=require",
		DatabaseCatalogOwnerRole: "kq_pg_catalog_owner",
		DatabaseOwnerRole:        "kq_cp_owner",
		DatabaseMigratorRole:     "kq_cp_migrator",
		DatabaseRuntimeRole:      "kq_cp_runtime",
		DatabaseValidatorRole:    "kq_cp_validator",
		DatabaseDedicatedCluster: true,
	}
	poolConfig, err := runtimeDatabasePoolConfig(cfg)
	if err != nil {
		t.Fatalf("runtime database pool config: %v", err)
	}
	if got := poolConfig.ConnConfig.RuntimeParams["search_path"]; got != "pg_catalog, public, pg_temp" {
		t.Fatalf("search_path=%q", got)
	}
	if poolConfig.BeforeAcquire == nil || poolConfig.AfterConnect == nil {
		t.Fatal("runtime pool omitted checkout identity verification")
	}
}

func TestRuntimeDatabasePoolConfigLeavesDevelopmentPoolUnattested(t *testing.T) {
	cfg := &config.Config{DatabaseURL: "postgres://dev:dev@localhost/dev?sslmode=disable"}
	poolConfig, err := runtimeDatabasePoolConfig(cfg)
	if err != nil {
		t.Fatalf("development database pool config: %v", err)
	}
	if poolConfig.AfterConnect != nil || poolConfig.BeforeAcquire != nil {
		t.Fatal("development pool unexpectedly received strict-role hooks")
	}
}

func TestDatabaseSecurityConfigMapsEveryTrustAnchor(t *testing.T) {
	cfg := &config.Config{
		DatabaseCatalogOwnerRole: "catalog_owner", DatabaseOwnerRole: "owner",
		DatabaseMigratorRole: "migrator", DatabaseRuntimeRole: "runtime",
		DatabaseValidatorRole: "validator", DatabaseDedicatedCluster: true,
	}
	got := databaseSecurityConfig(cfg)
	if got.CatalogOwnerRole != "catalog_owner" || got.OwnerRole != "owner" ||
		got.MigratorRole != "migrator" || got.RuntimeRole != "runtime" ||
		got.ValidatorRole != "validator" || !got.DedicatedCluster {
		t.Fatalf("database security mapping = %+v", got)
	}
}

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
func (mainTestProblemRepo) ListAll(ctx context.Context) ([]models.Problem, error) {
	return []models.Problem{{ID: "p1", Title: "One"}}, nil
}
func (mainTestProblemRepo) FindByID(ctx context.Context, id string) (*models.Problem, error) {
	return &models.Problem{ID: id}, nil
}
func (mainTestProblemRepo) FindAnyByID(ctx context.Context, id string) (*models.Problem, error) {
	return &models.Problem{ID: id}, nil
}
func (mainTestProblemRepo) Upsert(ctx context.Context, p *models.Problem) error { return nil }
func (mainTestProblemRepo) Delete(ctx context.Context, id string) error         { return nil }

type mainTestLoader struct{}

func (mainTestLoader) Sync(ctx context.Context) (int, error) { return 0, nil }
func (mainTestLoader) Healthy() bool                         { return true }

type startupSyncerStub struct {
	count int
	err   error
}

func (s startupSyncerStub) Startup(context.Context) (int, error) {
	return s.count, s.err
}

type mainTestSessionSvc struct{}

func (mainTestSessionSvc) StartProblem(ctx context.Context, userID, problemID string) (string, error) {
	return "s1", nil
}
func (mainTestSessionSvc) StartProblemOperation(ctx context.Context, userID, problemID, operationID string) (*session.CurrentSession, error) {
	return &session.CurrentSession{OperationID: operationID, SessionID: "s1", ProblemID: problemID, Generation: 1}, nil
}
func (mainTestSessionSvc) Verify(ctx context.Context, userID, operationID string) (bool, string, error) {
	return false, "", nil
}
func (mainTestSessionSvc) SubmitChoice(context.Context, string, string, runner.SessionRef, string, string) (bool, error) {
	return false, nil
}
func (mainTestSessionSvc) ResetEnvironment(ctx context.Context, userID string) error { return nil }
func (mainTestSessionSvc) ResetEnvironmentOperation(ctx context.Context, userID, problemID string, expected runner.SessionRef, operationID string) (*session.CurrentSession, error) {
	return &session.CurrentSession{OperationID: operationID, SessionID: "s1", ProblemID: problemID, Generation: 2}, nil
}
func (mainTestSessionSvc) GetCurrentSession(userID string) *session.CurrentSession { return nil }

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

func TestAPICORSAllowsIdempotencyKey(t *testing.T) {
	cfg := apiCORSConfig("https://quiz.example")
	found := false
	for _, header := range cfg.AllowHeaders {
		if strings.EqualFold(header, "Idempotency-Key") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("CORS allow headers omit Idempotency-Key: %v", cfg.AllowHeaders)
	}
}

func TestHTTPAccessLogNeverIncludesQueryString(t *testing.T) {
	var output bytes.Buffer
	router := newHTTPRouter(&output)
	router.GET("/api/auth/github/callback", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	router.GET("/ws/terminal", func(c *gin.Context) { c.Status(http.StatusUnauthorized) })

	markers := []string{"OAUTH_CODE_SENSITIVE", "WS_SESSION_SENSITIVE"}
	targets := []string{
		"/api/auth/github/callback?code=" + markers[0] + "&state=state",
		"/ws/terminal?token=" + markers[1] + "&session_id=session",
	}
	for _, target := range targets {
		request := httptest.NewRequest(http.MethodGet, target, nil)
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
	}
	logged := output.String()
	for _, marker := range markers {
		if strings.Contains(logged, marker) {
			t.Fatalf("query marker %q leaked in access log %q", marker, logged)
		}
	}
	if !strings.Contains(logged, "/api/auth/github/callback") || !strings.Contains(logged, "/ws/terminal") {
		t.Fatalf("access log omitted safe route paths: %q", logged)
	}
}

func TestHTTPRecoveryNeverIncludesQueryHeadersOrPanicValue(t *testing.T) {
	var output bytes.Buffer
	router := newHTTPRouter(&output)
	router.GET("/panic", func(*gin.Context) { panic("PANIC_VALUE_SENSITIVE") })
	request := httptest.NewRequest(http.MethodGet, "/panic?code=QUERY_SENSITIVE", nil)
	request.Header.Set("Authorization", "Bearer HEADER_SENSITIVE")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("panic status=%d", response.Code)
	}
	logged := output.String()
	for _, marker := range []string{"PANIC_VALUE_SENSITIVE", "QUERY_SENSITIVE", "HEADER_SENSITIVE"} {
		if strings.Contains(logged, marker) {
			t.Fatalf("recovery marker %q leaked in log %q", marker, logged)
		}
	}
	if !strings.Contains(logged, "path=/panic") || !strings.Contains(logged, "panic=recovered") {
		t.Fatalf("recovery omitted safe diagnostic: %q", logged)
	}
}

func TestLoadStartupCatalogDelegatesToSafeCoordinatorStartup(t *testing.T) {
	tests := []struct {
		name      string
		syncer    startupSyncerStub
		wantCount int
		wantErr   string
	}{
		{name: "coordinator error", syncer: startupSyncerStub{err: errors.New("replace failed")}, wantErr: "replace failed"},
		{name: "success", syncer: startupSyncerStub{count: 2}, wantCount: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := loadStartupCatalog(context.Background(), tc.syncer)
			if got != tc.wantCount {
				t.Fatalf("count = %d, want %d", got, tc.wantCount)
			}
			if tc.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("error = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestLocalRecoveryExpectationsBindExactDesiredAbsentIdentity(t *testing.T) {
	ref := runner.AllocationRef{
		ID:       runner.AllocationIDForSession(runner.SessionRef{SessionID: "session-recovery-1", Generation: 2}),
		Session:  runner.SessionRef{SessionID: "session-recovery-1", Generation: 2},
		Provider: runner.ProviderLocalDocker,
	}
	recovery := runner.LocalRecoveryAllocation{
		Ref: ref,
		Selection: runner.CatalogSelection{
			Generation: 7,
			Problem:    runner.ProblemRef{ID: "problem-recovery", Revision: strings.Repeat("a", 64)},
		},
		ResourceProfile: runner.DefaultResourceProfile,
	}
	expectations, err := localRecoveryExpectations([]runner.LocalRecoveryAllocation{recovery}, "home-dev")
	if err != nil || len(expectations) != 1 {
		t.Fatalf("local recovery expectations = %+v, %v", expectations, err)
	}
	want := runner.ReconcileExpectation{Ownership: runner.ReconcileOwnership{
		Provider: runner.ProviderLocalDocker, Scope: "home-dev", AllocationID: ref.ID,
		SessionID: ref.Session.SessionID, Generation: ref.Session.Generation,
	}, Desired: runner.ReconcileDesiredAbsent, Selection: recovery.Selection, ResourceProfile: recovery.ResourceProfile}
	if expectations[0] != want {
		t.Fatalf("local recovery expectation = %+v, want %+v", expectations[0], want)
	}
	if _, err := localRecoveryExpectations([]runner.LocalRecoveryAllocation{recovery, recovery}, "home-dev"); err == nil {
		t.Fatal("duplicate local recovery allocation was accepted")
	}
	stale := recovery
	stale.Ref.ID = "not-generation-bound"
	if _, err := localRecoveryExpectations([]runner.LocalRecoveryAllocation{stale}, "home-dev"); err == nil {
		t.Fatal("unbound local recovery allocation was accepted")
	}
	missingProvenance := recovery
	missingProvenance.Selection = runner.CatalogSelection{}
	if _, err := localRecoveryExpectations([]runner.LocalRecoveryAllocation{missingProvenance}, "home-dev"); err == nil {
		t.Fatal("recovery allocation without catalog provenance was accepted")
	}
	nonstandard := recovery
	nonstandard.ResourceProfile = "browser-selected"
	if _, err := localRecoveryExpectations([]runner.LocalRecoveryAllocation{nonstandard}, "home-dev"); err == nil {
		t.Fatal("unapproved recovery resource profile was accepted")
	}
}

func TestValidateLocalRecoveryAbsenceFailsClosed(t *testing.T) {
	ownership := runner.ReconcileOwnership{
		Provider: runner.ProviderLocalDocker, Scope: "home-dev",
		AllocationID: runner.AllocationIDForSession(runner.SessionRef{SessionID: "session-recovery-1", Generation: 1}),
		SessionID:    "session-recovery-1", Generation: 1,
	}
	expectations := []runner.ReconcileExpectation{{Ownership: ownership, Desired: runner.ReconcileDesiredAbsent}}
	missing := runner.ReconcileFinding{Kind: runner.ReconcileFindingMissing, Ownership: ownership, Desired: runner.ReconcileDesiredAbsent}
	cleanup := runner.ReconcileFinding{Kind: runner.ReconcileFindingCleanupRequired, Ownership: ownership, Desired: runner.ReconcileDesiredAbsent}
	observed := runner.ReconcileAction{Kind: runner.ReconcileActionObserved, Ownership: ownership}
	destroyed := runner.ReconcileAction{Kind: runner.ReconcileActionDestroyed, Ownership: ownership, Applied: true}

	valid := []runner.ReconcileResult{
		{Mode: runner.ReconcileModeApply, Findings: []runner.ReconcileFinding{missing}, Actions: []runner.ReconcileAction{observed}},
		{Mode: runner.ReconcileModeApply, Findings: []runner.ReconcileFinding{cleanup}, Actions: []runner.ReconcileAction{destroyed}},
	}
	for i, result := range valid {
		if err := validateLocalRecoveryAbsence(expectations, result); err != nil {
			t.Fatalf("valid absence proof %d rejected: %v", i, err)
		}
	}

	unexpected := ownership
	unexpected.AllocationID = runner.AllocationIDForSession(runner.SessionRef{SessionID: "other", Generation: 1})
	unexpected.SessionID = "other"
	invalid := []runner.ReconcileResult{
		{Mode: runner.ReconcileModeReportOnly, Findings: []runner.ReconcileFinding{missing}, Actions: []runner.ReconcileAction{observed}},
		{Mode: runner.ReconcileModeApply, Actions: []runner.ReconcileAction{observed}},
		{Mode: runner.ReconcileModeApply, Findings: []runner.ReconcileFinding{missing}},
		{Mode: runner.ReconcileModeApply, Findings: []runner.ReconcileFinding{missing}, Actions: []runner.ReconcileAction{{Kind: runner.ReconcileActionManualReview, Ownership: ownership}}},
		{Mode: runner.ReconcileModeApply, Findings: []runner.ReconcileFinding{{Kind: runner.ReconcileFindingAmbiguousOwnership, Ownership: ownership, Desired: runner.ReconcileDesiredAbsent}}, Actions: []runner.ReconcileAction{observed}},
		{Mode: runner.ReconcileModeApply, Findings: []runner.ReconcileFinding{missing, missing}, Actions: []runner.ReconcileAction{observed}},
		{Mode: runner.ReconcileModeApply, Findings: []runner.ReconcileFinding{cleanup}, Actions: []runner.ReconcileAction{observed}},
		{Mode: runner.ReconcileModeApply, Findings: []runner.ReconcileFinding{missing}, Actions: []runner.ReconcileAction{destroyed}},
		{Mode: runner.ReconcileModeApply, Findings: []runner.ReconcileFinding{missing}, Actions: []runner.ReconcileAction{{Kind: runner.ReconcileActionObserved, Ownership: unexpected}}},
	}
	for i, result := range invalid {
		if err := validateLocalRecoveryAbsence(expectations, result); err == nil {
			t.Fatalf("invalid absence proof %d was accepted: %+v", i, result)
		}
	}
}

func TestReadinessHandlerFollowsMonotonicAuthorityAndAdmission(t *testing.T) {
	gate := runner.NewAuthorityGate()
	accepting := true
	r := gin.New()
	r.GET("/live", livenessHandler)
	r.GET("/ready", readinessHandler(gate, func() bool { return accepting }))

	request := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		return w
	}
	if got := request("/live").Code; got != http.StatusOK {
		t.Fatalf("liveness status = %d", got)
	}
	if got := request("/ready").Code; got != http.StatusServiceUnavailable {
		t.Fatalf("recovering readiness status = %d", got)
	}
	if err := gate.Activate(); err != nil {
		t.Fatal(err)
	}
	if got := request("/ready").Code; got != http.StatusOK {
		t.Fatalf("ready status = %d", got)
	}
	accepting = false
	if got := request("/ready").Code; got != http.StatusServiceUnavailable {
		t.Fatalf("closed admission readiness status = %d", got)
	}
	accepting = true
	gate.BeginDrain()
	if got := request("/ready").Code; got != http.StatusServiceUnavailable {
		t.Fatalf("draining readiness status = %d", got)
	}
	gate.Fence(errors.New("lost"))
	if got := request("/ready").Code; got != http.StatusServiceUnavailable {
		t.Fatalf("fenced readiness status = %d", got)
	}
}
