package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/k8s-quiz/backend/internal/auth"
	"github.com/k8s-quiz/backend/internal/container"
	"github.com/k8s-quiz/backend/internal/problem"
	"github.com/k8s-quiz/backend/internal/runner"
	"github.com/k8s-quiz/backend/internal/session"
	"github.com/k8s-quiz/backend/internal/user"
	"github.com/k8s-quiz/backend/internal/ws"
	"github.com/k8s-quiz/backend/pkg/config"
	"github.com/k8s-quiz/backend/pkg/dbmigrate"
	"github.com/k8s-quiz/backend/pkg/dbsecurity"
	"github.com/k8s-quiz/backend/pkg/llm"
	"github.com/k8s-quiz/backend/pkg/middleware"
)

func main() {
	cfg := config.Load()

	if err := cfg.Validate(); err != nil {
		log.Fatalf("insecure configuration: %v", err)
	}

	// SEC3-11: quiet gin debug output outside local development.
	if !cfg.IsLocal() {
		gin.SetMode(gin.ReleaseMode)
	}

	databasePoolConfig, err := runtimeDatabasePoolConfig(cfg)
	if err != nil {
		log.Fatalf("invalid runtime database configuration: %v", err)
	}
	db, err := pgxpool.NewWithConfig(context.Background(), databasePoolConfig)
	if err != nil {
		log.Fatalf("failed to connect to database: %v", err)
	}
	defer db.Close()

	if err := db.Ping(context.Background()); err != nil {
		log.Fatalf("failed to ping database: %v", err)
	}

	if cfg.DatabaseMigrationMode == "external" {
		if err := dbmigrate.RequireCurrent(context.Background(), db); err != nil {
			log.Fatalf("database schema is not ready; run the one-shot migrator: %v", err)
		}
	} else if err := dbmigrate.Up(cfg.DatabaseURL); err != nil {
		// Development convenience only. Config validation forbids this branch
		// in public mode so a compromised server never gains migrator authority.
		log.Fatalf("failed to apply development migrations: %v", err)
	}
	// Attest the complete role topology, ownership, exact grants and effective
	// runtime denials before acquiring controller authority. The server never
	// receives the migrator, provisioner or validator credential.
	if cfg.DatabaseSecurityConfigured() {
		if err := dbsecurity.VerifyRuntime(context.Background(), db, databaseSecurityConfig(cfg)); err != nil {
			log.Fatalf("runtime database security attestation failed: %v", err)
		}
	}
	providerID := cfg.RunnerProvider + ":" + cfg.RunnerScope
	authorityGate := runner.NewAuthorityGate()
	controllerLease, err := runner.AcquireControllerLease(context.Background(), db, providerID, authorityGate.Fence)
	if err != nil {
		log.Fatalf("failed to acquire runner controller lease: %v", err)
	}
	defer func() {
		leaseCloseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := controllerLease.Close(leaseCloseCtx); err != nil {
			log.Printf("release runner controller lease: %v", err)
		}
	}()
	controllerFence := controllerLease.Fence()
	runnerStore, err := runner.NewPostgresStore(db, controllerFence)
	if err != nil {
		log.Fatalf("failed to create durable runner store: %v", err)
	}

	userRepo := user.NewRepository(db)
	problemRepo := problem.NewRepository(db)

	var runtimeRunner runner.Runner
	var problemLoader *problem.GitLoader
	var problemCatalog *problem.CatalogCoordinator
	switch cfg.RunnerProvider {
	case string(runner.ProviderLocalDocker):
		dockerMgr, err := container.NewDockerManager(cfg.DockerHost)
		if err != nil {
			log.Fatalf("failed to create docker manager: %v", err)
		}
		artifactStorePath, err := filepath.Abs(cfg.ProblemArtifactStorePath)
		if err != nil {
			log.Fatalf("failed to resolve problem artifact store path: %v", err)
		}
		artifactStore, err := problem.NewFilesystemArtifactStore(artifactStorePath)
		if err != nil {
			log.Fatalf("failed to open problem artifact store: %v", err)
		}
		defer func() {
			if err := artifactStore.Close(); err != nil {
				log.Printf("close problem artifact store: %v", err)
			}
		}()
		problemLoader, err = problem.NewPersistentRuntimeGitLoader(cfg.ProblemsRepoPath, dockerMgr, artifactStore)
		if err != nil {
			log.Fatalf("failed to create runtime problem loader: %v", err)
		}
		problemCatalog, err = problem.NewCatalogCoordinator(problemLoader, problemRepo)
		if err != nil {
			log.Fatalf("failed to create problem catalog coordinator: %v", err)
		}
		developmentVerifier, err := runner.NewDevelopmentGuestVerifier(dockerMgr)
		if err != nil {
			log.Fatalf("failed to create development guest verifier: %v", err)
		}
		localRunner, err := runner.NewLocalDockerRunner(dockerMgr, problemCatalog, developmentVerifier, cfg.RunnerScope)
		if err != nil {
			log.Fatalf("failed to create local Docker runner: %v", err)
		}
		runtimeRunner = localRunner
	default:
		// Config.Validate rejects every unimplemented provider before DB or
		// Docker setup. Keep this guard so future wiring cannot add fallback.
		log.Fatalf("runner provider %q has no implementation", cfg.RunnerProvider)
	}
	authorizedRunner, err := runner.NewAuthorityRunner(runtimeRunner, authorityGate, controllerFence)
	if err != nil {
		log.Fatalf("failed to bind runner controller authority: %v", err)
	}
	recoveryCtx, cancelRecovery, err := authorityGate.RecoveryContext(context.Background())
	if err != nil {
		log.Fatalf("failed to bind startup recovery authority: %v", err)
	}
	defer cancelRecovery()

	// Local Docker is development-only and is never adopted across a process
	// restart. Freeze the durable work before provider cleanup, prove scoped
	// resources absent, then converge exactly that snapshot before admission.
	localRecoverySnapshot, err := runnerStore.SnapshotLocalRecovery(recoveryCtx, providerID)
	if err != nil {
		log.Fatalf("failed to snapshot durable local runner recovery work: %v", err)
	}
	// Terminalize controller-lost verify calls before the Local Docker policy
	// marks every development allocation absent. This preserves the typed
	// infrastructure replay result and keyed verify_finished event.
	if err := runnerStore.PrepareRecoveryWork(recoveryCtx, providerID); err != nil {
		log.Fatalf("failed to prepare durable local runner recovery work: %v", err)
	}
	if len(localRecoverySnapshot) != 0 {
		expectations, err := localRecoveryExpectations(localRecoverySnapshot, cfg.RunnerScope)
		if err != nil {
			log.Fatalf("failed to bind exact local recovery expectations: %v", err)
		}
		reconcileResult, err := authorizedRunner.ReconcileRecovery(recoveryCtx, runner.ReconcileRequest{
			Mode: runner.ReconcileModeApply, Expectations: expectations,
		})
		if err != nil {
			log.Fatalf("failed to reconcile stale development runner resources: %v", err)
		}
		if err := validateLocalRecoveryAbsence(expectations, reconcileResult); err != nil {
			log.Fatalf("local runner cleanup did not prove exact absence: %v", err)
		}
		if _, err := runnerStore.ConvergeLocalRestartSnapshot(recoveryCtx, providerID, localRecoverySnapshot); err != nil {
			log.Fatalf("failed to converge durable local runner recovery state: %v", err)
		}
	}
	// Catalog startup happens only after exact recovery. Legacy allocations with
	// NULL catalog provenance fail SnapshotLocalRecovery above and require manual
	// review; startup never guesses a destructive create specification.
	if count, err := loadStartupCatalog(recoveryCtx, problemCatalog); err != nil {
		log.Fatalf("failed to bootstrap or adopt the approved problem catalog: %v", err)
	} else {
		log.Printf("bootstrapped or adopted %d approved problems", count)
	}

	authService := auth.NewService(cfg, db, userRepo)
	sessionSvc, err := session.NewDurablePausedService(authorizedRunner, problemRepo, problemCatalog, runnerStore, providerID)
	if err != nil {
		log.Fatalf("failed to create durable session service: %v", err)
	}
	if err := sessionSvc.SetAuthorityGate(authorityGate); err != nil {
		log.Fatalf("failed to bind session controller authority: %v", err)
	}
	sessionSvc.SetMaxConcurrentSessions(cfg.MaxConcurrentSessions)
	// Reconstruct the provider-neutral durable projection and drain every
	// immediately claimable lifecycle operation before any public admission.
	// Local Docker has already converged its destructive restart policy above;
	// an adoptable Proxmox/cloud provider will use the same paused recovery gate.
	if err := sessionSvc.RecoverDurableState(recoveryCtx); err != nil {
		log.Fatalf("failed to recover durable session state before admission: %v", err)
	}
	cancelRecovery()

	hub := ws.NewHub()
	go hub.Run()
	sessionSvc.SetGrader(llm.NewClient(cfg.LLMAPIKey, cfg.LLMModel, cfg.LLMBaseURL))

	if err := authorityGate.Activate(); err != nil {
		log.Fatalf("failed to activate runner controller authority after recovery: %v", err)
	}
	if err := sessionSvc.Activate(); err != nil {
		log.Fatalf("failed to activate session service after recovery: %v", err)
	}

	r := newHTTPRouter(os.Stdout)

	r.Use(cors.New(apiCORSConfig(cfg.FrontendURL)))

	api := r.Group("/api")

	authHandler := auth.NewHandler(authService)
	authHandler.RegisterRoutes(api)

	protected := api.Group("")
	protected.Use(middleware.Auth(authService))

	problemHandler := problem.NewHandler(problemRepo, problemCatalog, sessionSvc)
	problemHandler.RegisterRoutes(protected)
	problemHandler.RegisterAdminRoutes(protected.Group("", middleware.AdminOnly()))

	userHandler := user.NewHandler(userRepo, problemRepo)
	userHandler.RegisterRoutes(protected)
	userHandler.RegisterAdminRoutes(protected.Group("", middleware.AdminOnly()))

	sessionHandler := session.NewHandler(sessionSvc)
	sessionHandler.RegisterRoutes(protected)

	terminalHandler := ws.NewTerminalHandler(hub, authService, authorizedRunner, sessionSvc.GetTerminalTarget, sessionSvc.AcquireTerminalCommit, sessionSvc.IsAccepting, authorityGate.Context(), cfg.FrontendURL)
	r.GET("/ws/terminal", terminalHandler.HandleWebSocket)
	lifecycleSocketCtx, cancelLifecycleSockets := context.WithCancel(context.Background())
	defer cancelLifecycleSockets()
	lifecycleHandler := ws.NewLifecycleHandler(runnerStore, authService, lifecycleSocketCtx, cfg.FrontendURL)
	r.GET("/ws/lifecycle", lifecycleHandler.HandleWebSocket)

	r.GET("/live", livenessHandler)
	catalogAccepting := func() bool { return sessionSvc.IsAccepting() && problemCatalog.Healthy() }
	r.GET("/ready", readinessHandler(authorityGate, catalogAccepting))
	r.GET("/health", readinessHandler(authorityGate, catalogAccepting))

	srv := &http.Server{
		Addr:              net.JoinHostPort(cfg.ServerHost, cfg.ServerPort),
		Handler:           r,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	go func() {
		log.Printf("server starting on %s", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	leaseLost := false
	select {
	case sig := <-quit:
		log.Printf("received shutdown signal %s", sig)
	case err := <-controllerLease.Lost():
		leaseLost = true
		log.Printf("runner controller lease lost; stopping admission: %v", err)
	}

	log.Println("shutting down...")
	if !leaseLost {
		authorityGate.BeginDrain()
	}
	sessionSvc.BeginShutdown()
	// WebSockets are hijacked connections and http.Server.Shutdown does not
	// close them. Lifecycle reads are independent of provider admission while
	// draining, but must stop explicitly when this process exits.
	cancelLifecycleSockets()
	// Terminal sockets carry only terminal data. Lifecycle clients converge
	// from PostgreSQL after reconnect instead of trusting an ephemeral restart
	// frame from this process.
	hub.CloseAll()

	httpShutdownCtx, cancelHTTPShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	if err := srv.Shutdown(httpShutdownCtx); err != nil {
		log.Printf("HTTP shutdown completed with error: %v", err)
	}
	cancelHTTPShutdown()

	provisioningCtx, cancelProvisioningWait := context.WithTimeout(context.Background(), 30*time.Second)
	if err := sessionSvc.WaitForProvisioning(provisioningCtx); err != nil {
		log.Printf("waiting for in-flight provisioning failed: %v", err)
	}
	cancelProvisioningWait()

	watcherCtx, cancelWatcherWait := context.WithTimeout(context.Background(), 5*time.Second)
	if err := sessionSvc.WaitForWatchers(watcherCtx); err != nil {
		log.Printf("waiting for session watchers failed: %v", err)
	}
	cancelWatcherWait()

	if !leaseLost {
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 30*time.Second)
		if err := sessionSvc.CleanupAll(cleanupCtx); err != nil {
			log.Printf("session cleanup completed with errors: %v", err)
		}
		cancelCleanup()
	} else {
		log.Printf("skipping provider cleanup after controller lease loss; the next controller will reconcile durable cleanup intent")
	}
}

func localRecoveryExpectations(snapshot []runner.LocalRecoveryAllocation, scope string) ([]runner.ReconcileExpectation, error) {
	if scope == "" {
		return nil, errors.New("local recovery scope is required")
	}
	expectations := make([]runner.ReconcileExpectation, 0, len(snapshot))
	seen := make(map[runner.ReconcileOwnership]struct{}, len(snapshot))
	for _, recovery := range snapshot {
		ref := recovery.Ref
		ownership := runner.ReconcileOwnership{
			Provider: ref.Provider, Scope: scope, AllocationID: ref.ID,
			SessionID: ref.Session.SessionID, Generation: ref.Session.Generation,
		}
		if ref.Provider != runner.ProviderLocalDocker || ref.ID == "" || ref.Session.SessionID == "" || ref.Session.Generation == 0 ||
			ref.ID != runner.AllocationIDForSession(ref.Session) || recovery.Selection.Generation == 0 ||
			recovery.Selection.Problem.ID == "" || recovery.Selection.Problem.Revision == "" ||
			recovery.ResourceProfile != runner.DefaultResourceProfile {
			return nil, fmt.Errorf("invalid local recovery allocation %+v", ref)
		}
		if _, duplicate := seen[ownership]; duplicate {
			return nil, fmt.Errorf("duplicate local recovery allocation %s", ref.ID)
		}
		seen[ownership] = struct{}{}
		expectations = append(expectations, runner.ReconcileExpectation{
			Ownership: ownership, Desired: runner.ReconcileDesiredAbsent,
			Selection: recovery.Selection, ResourceProfile: recovery.ResourceProfile,
		})
	}
	return expectations, nil
}

// validateLocalRecoveryAbsence is the fail-closed boundary between provider
// cleanup and durable convergence. Only exact observations made by the
// authoritative reconciliation call may authorize PostgreSQL absence.
func validateLocalRecoveryAbsence(expectations []runner.ReconcileExpectation, result runner.ReconcileResult) error {
	if result.Mode != runner.ReconcileModeApply {
		return fmt.Errorf("reconcile mode is %q, want apply", result.Mode)
	}
	want := make(map[runner.ReconcileOwnership]struct{}, len(expectations))
	for _, expectation := range expectations {
		if expectation.Desired != runner.ReconcileDesiredAbsent {
			return fmt.Errorf("recovery expectation for %s is not desired-absent", expectation.Ownership.AllocationID)
		}
		if _, duplicate := want[expectation.Ownership]; duplicate {
			return fmt.Errorf("duplicate recovery expectation for %s", expectation.Ownership.AllocationID)
		}
		want[expectation.Ownership] = struct{}{}
	}
	findings := make(map[runner.ReconcileOwnership]runner.ReconcileFindingKind, len(expectations))
	for _, finding := range result.Findings {
		if _, expected := want[finding.Ownership]; !expected {
			return fmt.Errorf("unexpected reconciliation finding %q for allocation %s", finding.Kind, finding.Ownership.AllocationID)
		}
		if finding.Desired != runner.ReconcileDesiredAbsent {
			return fmt.Errorf("reconciliation finding for %s is not desired-absent", finding.Ownership.AllocationID)
		}
		if finding.Kind != runner.ReconcileFindingMissing && finding.Kind != runner.ReconcileFindingCleanupRequired {
			return fmt.Errorf("reconciliation finding %q for %s does not prove absence", finding.Kind, finding.Ownership.AllocationID)
		}
		if _, duplicate := findings[finding.Ownership]; duplicate {
			return fmt.Errorf("duplicate reconciliation finding for allocation %s", finding.Ownership.AllocationID)
		}
		findings[finding.Ownership] = finding.Kind
	}
	proved := make(map[runner.ReconcileOwnership]struct{}, len(expectations))
	for _, action := range result.Actions {
		if _, expected := want[action.Ownership]; !expected {
			return fmt.Errorf("unexpected reconciliation action for allocation %s", action.Ownership.AllocationID)
		}
		finding, found := findings[action.Ownership]
		if !found {
			return fmt.Errorf("reconciliation action for %s has no matching finding", action.Ownership.AllocationID)
		}
		switch action.Kind {
		case runner.ReconcileActionObserved:
			if action.Applied {
				return fmt.Errorf("observed-absent action for %s is incorrectly marked applied", action.Ownership.AllocationID)
			}
			if finding != runner.ReconcileFindingMissing {
				return fmt.Errorf("observed-absent action for %s does not match finding %q", action.Ownership.AllocationID, finding)
			}
		case runner.ReconcileActionDestroyed:
			if !action.Applied {
				return fmt.Errorf("destroyed action for %s lacks applied absence proof", action.Ownership.AllocationID)
			}
			if finding != runner.ReconcileFindingCleanupRequired {
				return fmt.Errorf("destroyed action for %s does not match finding %q", action.Ownership.AllocationID, finding)
			}
		default:
			return fmt.Errorf("reconciliation action %q for %s does not prove absence", action.Kind, action.Ownership.AllocationID)
		}
		if _, duplicate := proved[action.Ownership]; duplicate {
			return fmt.Errorf("duplicate absence proof for allocation %s", action.Ownership.AllocationID)
		}
		proved[action.Ownership] = struct{}{}
	}
	if len(findings) != len(want) || len(proved) != len(want) {
		return fmt.Errorf("absence findings/actions cover %d/%d of %d exact allocations", len(findings), len(proved), len(want))
	}
	return nil
}

func livenessHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "alive"})
}

func readinessHandler(gate *runner.AuthorityGate, accepting func() bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		if gate == nil || accepting == nil || !gate.Ready() || !accepting() {
			state := runner.AuthorityFenced
			if gate != nil {
				state = gate.State()
			}
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not_ready", "authority": state})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ready"})
	}
}

func apiCORSConfig(frontendURL string) cors.Config {
	return cors.Config{
		AllowOrigins:     []string{frontendURL},
		AllowMethods:     []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Authorization", "Idempotency-Key"},
		AllowCredentials: true,
		MaxAge:           12 * time.Hour,
	}
}

func newHTTPRouter(output io.Writer) *gin.Engine {
	if output == nil {
		output = io.Discard
	}
	router := gin.New()
	router.Use(gin.LoggerWithConfig(gin.LoggerConfig{
		Output: output,
		Formatter: func(params gin.LogFormatterParams) string {
			// Never include RawQuery. OAuth codes, WebSocket access tokens and
			// session identifiers are capabilities or private metadata even when
			// their lifetime is short.
			return fmt.Sprintf("%s method=%s path=%s status=%d latency=%s size=%d\n",
				params.TimeStamp.UTC().Format(time.RFC3339), params.Method,
				params.Request.URL.Path, params.StatusCode, params.Latency, params.BodySize)
		},
	}), redactedRecovery(output))
	return router
}

func redactedRecovery(output io.Writer) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if recover() == nil {
				return
			}
			// Do not dump the request line, headers, panic value, or body. They may
			// contain OAuth codes, bearer tokens, cookies, or learner input.
			_, _ = fmt.Fprintf(output, "%s method=%s path=%s status=500 panic=recovered\n",
				time.Now().UTC().Format(time.RFC3339), c.Request.Method, c.Request.URL.Path)
			c.AbortWithStatus(http.StatusInternalServerError)
		}()
		c.Next()
	}
}

type startupProblemSyncer interface {
	Startup(context.Context) (int, error)
}

func loadStartupCatalog(ctx context.Context, syncer startupProblemSyncer) (int, error) {
	return syncer.Startup(ctx)
}
