package config

import (
	"fmt"
	"log"
	"net/url"
	"os"
	"strconv"
	"strings"
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

// knownWeakSecrets are values that ship in examples/compose and must never be
// used outside local development.
var knownWeakSecrets = map[string]bool{
	"":                                     true,
	"dev-secret-change-me":                 true,
	"dev-refresh-secret-change-me":         true,
	"change-this-to-random-string":         true,
	"change-this-to-another-random-string": true,
}

func weakSecret(s string) bool {
	return knownWeakSecrets[s] || len(s) < 16
}

func isLocalURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return h == "" || h == "localhost" || h == "127.0.0.1" || h == "::1"
}

// Validate refuses to start in a non-local deployment that still uses an
// unset or publicly-known JWT secret (which would let anyone forge admin
// tokens). Local development (FRONTEND_URL on localhost) is allowed with a
// loud warning so the demo keeps working out of the box.
func (c *Config) Validate() error {
	local := isLocalURL(c.FrontendURL)
	if weakSecret(c.JWTSecret) {
		if !local {
			return fmt.Errorf("JWT_SECRET is unset or a known-weak default while FRONTEND_URL=%q is not local; set a strong random secret (>=16 bytes)", c.FrontendURL)
		}
		log.Printf("SECURITY WARNING: JWT_SECRET is a weak/default value; this is only acceptable for local development (FRONTEND_URL=%q).", c.FrontendURL)
	}
	if weakSecret(c.JWTRefreshSecret) {
		if !local {
			return fmt.Errorf("JWT_REFRESH_SECRET is unset or a known-weak default while FRONTEND_URL=%q is not local; set a strong random secret", c.FrontendURL)
		}
		log.Printf("SECURITY WARNING: JWT_REFRESH_SECRET is a weak/default value; local development only.")
	}
	return nil
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
