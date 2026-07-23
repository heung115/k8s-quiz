package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/k8s-quiz/backend/internal/auth"
	"github.com/k8s-quiz/backend/internal/container"
	"github.com/k8s-quiz/backend/internal/problem"
	"github.com/k8s-quiz/backend/internal/session"
	"github.com/k8s-quiz/backend/internal/user"
	"github.com/k8s-quiz/backend/internal/ws"
	"github.com/k8s-quiz/backend/pkg/config"
	"github.com/k8s-quiz/backend/pkg/llm"
	"github.com/k8s-quiz/backend/pkg/middleware"
)

func main() {
	cfg := config.Load()

	db, err := pgxpool.New(context.Background(), cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("failed to connect to database: %v", err)
	}
	defer db.Close()

	if err := db.Ping(context.Background()); err != nil {
		log.Fatalf("failed to ping database: %v", err)
	}

	dockerMgr, err := container.NewDockerManager(cfg.DockerHost)
	if err != nil {
		log.Fatalf("failed to create docker manager: %v", err)
	}

	userRepo := user.NewRepository(db)
	problemRepo := problem.NewRepository(db)
	problemLoader := problem.NewGitLoader(cfg.ProblemsRepoPath)

	authService := auth.NewService(cfg, db, userRepo)
	sessionSvc := session.NewService(dockerMgr, problemRepo, problemLoader)

	hub := ws.NewHub()
	go hub.Run()

	sessionSvc.SetStageCallback(func(userID, stage, message string) {
		hub.SendToUser(userID, ws.Message{
			Type:    ws.MsgStage,
			Stage:   stage,
			Message: message,
		})
	})
	sessionSvc.SetTimeoutCallback(func(userID string) {
		hub.SendToUser(userID, ws.Message{
			Type:   ws.MsgSessionEnded,
			Reason: "timeout",
		})
	})
	sessionSvc.SetTimeoutWarningCallback(func(userID string, remainingSeconds int) {
		hub.SendToUser(userID, ws.Message{
			Type:             ws.MsgTimeoutWarn,
			RemainingSeconds: remainingSeconds,
		})
	})
	sessionSvc.SetVerifyCallback(func(userID string, success bool, verifyLog string) {
		hub.SendToUser(userID, ws.Message{
			Type:    ws.MsgVerifyResult,
			Success: success,
			Log:     verifyLog,
		})
	})
	sessionSvc.SetCrashCallback(func(userID string) {
		hub.SendToUser(userID, ws.Message{
			Type:   ws.MsgSessionEnded,
			Reason: "container_crashed",
		})
	})
	sessionSvc.SetGrader(llm.NewClient(cfg.LLMAPIKey, cfg.LLMModel, cfg.LLMBaseURL))

	dockerMgr.RemoveByLabel(context.Background(), "k8s-quiz", "true")

	var warmPool *container.Pool
	if cfg.PoolSize > 0 {
		warmPool = container.NewPool(dockerMgr, cfg.PoolImage, cfg.PoolSize)
		sessionSvc.SetPool(warmPool)
		log.Printf("container pool enabled: size=%d image=%s", cfg.PoolSize, cfg.PoolImage)
	}

	r := gin.Default()

	r.Use(cors.New(cors.Config{
		AllowOrigins:     []string{cfg.FrontendURL},
		AllowMethods:     []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowHeaders:     []string{"Origin", "Content-Type", "Authorization"},
		AllowCredentials: true,
		MaxAge:           12 * time.Hour,
	}))

	api := r.Group("/api")

	authHandler := auth.NewHandler(authService)
	authHandler.RegisterRoutes(api)

	protected := api.Group("")
	protected.Use(middleware.Auth(authService))

	problemHandler := problem.NewHandler(problemRepo, problemLoader, sessionSvc)
	problemHandler.RegisterRoutes(protected)
	problemHandler.RegisterAdminRoutes(protected.Group("", middleware.AdminOnly()))

	userHandler := user.NewHandler(userRepo, problemRepo)
	userHandler.RegisterRoutes(protected)
	userHandler.RegisterAdminRoutes(protected.Group("", middleware.AdminOnly()))

	sessionHandler := session.NewHandler(sessionSvc)
	sessionHandler.RegisterRoutes(protected)

	terminalHandler := ws.NewTerminalHandler(hub, authService, dockerMgr, func(userID string) string {
		sess := sessionSvc.GetSession(userID)
		if sess == nil {
			return ""
		}
		return sess.ContainerID
	})
	r.GET("/ws/terminal", terminalHandler.HandleWebSocket)

	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})

	go func() {
		problems, err := problemLoader.LoadAll(context.Background())
		if err != nil {
			log.Printf("warning: failed to load problems on startup: %v", err)
			return
		}
		for i := range problems {
			problemRepo.Upsert(context.Background(), &problems[i])
		}
		log.Printf("synced %d problems on startup", len(problems))
	}()

	srv := &http.Server{
		Addr:    ":" + cfg.ServerPort,
		Handler: r,
	}

	go func() {
		log.Printf("server starting on :%s", cfg.ServerPort)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("shutting down...")
	sessionSvc.CleanupAll(context.Background())
	if warmPool != nil {
		warmPool.Drain(context.Background())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
}
