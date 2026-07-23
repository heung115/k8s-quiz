package config

import (
	"os"
	"strconv"
)

type Config struct {
	ServerPort         string
	DatabaseURL        string
	GithubClientID     string
	GithubClientSecret string
	JWTSecret          string
	JWTRefreshSecret   string
	ProblemsRepoPath   string
	FrontendURL        string
	DockerHost         string
	LLMAPIKey          string
	LLMModel           string
	LLMBaseURL         string
	PoolSize           int
	PoolImage          string
}

func Load() *Config {
	return &Config{
		ServerPort:         getEnv("SERVER_PORT", "8080"),
		DatabaseURL:        getEnv("DATABASE_URL", "postgres://k8squiz:k8squiz@localhost:5432/k8squiz?sslmode=disable"),
		GithubClientID:     getEnv("GITHUB_CLIENT_ID", ""),
		GithubClientSecret: getEnv("GITHUB_CLIENT_SECRET", ""),
		JWTSecret:          getEnv("JWT_SECRET", "dev-secret-change-me"),
		JWTRefreshSecret:   getEnv("JWT_REFRESH_SECRET", "dev-refresh-secret-change-me"),
		ProblemsRepoPath:   getEnv("PROBLEMS_REPO_PATH", "./problems"),
		FrontendURL:        getEnv("FRONTEND_URL", "http://localhost:5173"),
		DockerHost:         getEnv("DOCKER_HOST", "unix:///var/run/docker.sock"),
		LLMAPIKey:          getEnv("LLM_API_KEY", ""),
		LLMModel:           getEnv("LLM_MODEL", "gpt-4o-mini"),
		LLMBaseURL:         getEnv("LLM_BASE_URL", "https://api.openai.com/v1"),
		PoolSize:           getEnvInt("POOL_SIZE", 0),
		PoolImage:          getEnv("POOL_IMAGE", "k3s-base:latest"),
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}
