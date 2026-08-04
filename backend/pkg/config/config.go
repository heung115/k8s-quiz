package config

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

const maxSecretFileBytes = 64 * 1024

type Config struct {
	DeploymentMode           string
	ServerBindMode           string
	ServerHost               string
	ServerPort               string
	DatabaseURL              string
	DatabaseMigrationMode    string
	DatabaseCatalogOwnerRole string
	DatabaseOwnerRole        string
	DatabaseMigratorRole     string
	DatabaseRuntimeRole      string
	DatabaseValidatorRole    string
	DatabaseDedicatedCluster bool
	GithubClientID           string
	GithubClientSecret       string
	JWTSecret                string
	JWTRefreshSecret         string
	ProblemsRepoPath         string
	ProblemArtifactStorePath string
	FrontendURL              string
	RunnerProvider           string
	RunnerScope              string
	DockerHost               string
	LLMAPIKey                string
	LLMModel                 string
	LLMBaseURL               string
	PoolSize                 int
	PoolImage                string
	MaxConcurrentSessions    int
	maxConcurrentSessionsErr error
	databaseDedicatedErr     error
	secretLoadErr            error
	databaseURLFromFile      bool
	githubSecretFromFile     bool
	jwtSecretFromFile        bool
	jwtRefreshFromFile       bool
	llmAPIKeyFromFile        bool
}

func Load() *Config {
	maxConcurrentSessions, maxConcurrentSessionsErr := getEnvIntStrict("MAX_CONCURRENT_SESSIONS", 0)
	databaseDedicatedCluster, databaseDedicatedErr := getEnvBoolStrict("DATABASE_DEDICATED_CLUSTER", false)
	databaseURL, databaseURLFromFile, databaseURLErr := getEnvOrSecretFile(
		"DATABASE_URL", "DATABASE_URL_FILE", "postgres://k8squiz:k8squiz@localhost:5432/k8squiz?sslmode=disable",
	)
	githubClientSecret, githubSecretFromFile, githubSecretErr := getEnvOrSecretFile(
		"GITHUB_CLIENT_SECRET", "GITHUB_CLIENT_SECRET_FILE", "",
	)
	jwtSecret, jwtSecretFromFile, jwtSecretErr := getEnvOrSecretFile(
		"JWT_SECRET", "JWT_SECRET_FILE", "dev-secret-change-me",
	)
	jwtRefreshSecret, jwtRefreshFromFile, jwtRefreshErr := getEnvOrSecretFile(
		"JWT_REFRESH_SECRET", "JWT_REFRESH_SECRET_FILE", "dev-refresh-secret-change-me",
	)
	llmAPIKey, llmAPIKeyFromFile, llmAPIKeyErr := getEnvOrSecretFile(
		"LLM_API_KEY", "LLM_API_KEY_FILE", "",
	)
	return &Config{
		DeploymentMode:           getEnv("DEPLOYMENT_MODE", "development"),
		ServerBindMode:           getEnv("SERVER_BIND_MODE", "loopback"),
		ServerHost:               getEnv("SERVER_HOST", "127.0.0.1"),
		ServerPort:               getEnv("SERVER_PORT", "8080"),
		DatabaseURL:              databaseURL,
		DatabaseMigrationMode:    getEnv("DATABASE_MIGRATION_MODE", "auto"),
		DatabaseCatalogOwnerRole: getEnv("DATABASE_CATALOG_OWNER_ROLE", ""),
		DatabaseOwnerRole:        getEnv("DATABASE_OWNER_ROLE", ""),
		DatabaseMigratorRole:     getEnv("DATABASE_MIGRATOR_ROLE", ""),
		DatabaseRuntimeRole:      getEnv("DATABASE_RUNTIME_ROLE", ""),
		DatabaseValidatorRole:    getEnv("DATABASE_VALIDATOR_ROLE", ""),
		DatabaseDedicatedCluster: databaseDedicatedCluster,
		GithubClientID:           getEnv("GITHUB_CLIENT_ID", ""),
		GithubClientSecret:       githubClientSecret,
		JWTSecret:                jwtSecret,
		JWTRefreshSecret:         jwtRefreshSecret,
		ProblemsRepoPath:         getEnv("PROBLEMS_REPO_PATH", "./problems"),
		ProblemArtifactStorePath: getEnv("PROBLEM_ARTIFACT_STORE_PATH", "./data/problem-artifacts"),
		FrontendURL:              getEnv("FRONTEND_URL", "http://localhost:5173"),
		RunnerProvider:           getEnv("RUNNER_PROVIDER", "local-docker"),
		RunnerScope:              getEnv("RUNNER_SCOPE", "k8s-quiz-dev"),
		DockerHost:               getEnv("DOCKER_HOST", "unix:///var/run/docker.sock"),
		LLMAPIKey:                llmAPIKey,
		LLMModel:                 getEnv("LLM_MODEL", "gpt-4o-mini"),
		LLMBaseURL:               getEnv("LLM_BASE_URL", "https://api.openai.com/v1"),
		PoolSize:                 getEnvInt("POOL_SIZE", 0),
		PoolImage:                getEnv("POOL_IMAGE", "k3s-base:latest"),
		MaxConcurrentSessions:    maxConcurrentSessions,
		maxConcurrentSessionsErr: maxConcurrentSessionsErr,
		databaseDedicatedErr:     databaseDedicatedErr,
		secretLoadErr:            errors.Join(databaseURLErr, githubSecretErr, jwtSecretErr, jwtRefreshErr, llmAPIKeyErr),
		databaseURLFromFile:      databaseURLFromFile,
		githubSecretFromFile:     githubSecretFromFile,
		jwtSecretFromFile:        jwtSecretFromFile,
		jwtRefreshFromFile:       jwtRefreshFromFile,
		llmAPIKeyFromFile:        llmAPIKeyFromFile,
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
	// Fail CLOSED (SEC3-1): only an explicit http/https URL whose hostname is
	// a loopback name counts as local. Scheme-less values like
	// "prod.example.com" parse with an empty scheme/path-only URL and must
	// NOT be treated as local (that would bypass the weak-secret guard,
	// enable dev-login, and drop the Secure cookie flag).
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return h == "localhost" || h == "127.0.0.1" || h == "::1"
}

// IsLocal reports whether FRONTEND_URL points at localhost. It gates
// dev-only surfaces such as POST /api/auth/dev-login.
func (c *Config) IsLocal() bool {
	return isLocalURL(c.FrontendURL)
}

// CookieSecure reports whether auth cookies need the Secure flag: true iff
// FRONTEND_URL is served over https.
func (c *Config) CookieSecure() bool {
	return strings.HasPrefix(c.FrontendURL, "https://")
}

// Validate refuses to start in a non-local deployment that still uses an
// unset or publicly-known JWT secret (which would let anyone forge admin
// tokens). Local development (FRONTEND_URL on localhost) is allowed with a
// loud warning so the demo keeps working out of the box.
func (c *Config) Validate() error {
	local := isLocalURL(c.FrontendURL)
	switch c.DeploymentMode {
	case "development", "public":
	default:
		return fmt.Errorf("DEPLOYMENT_MODE=%q is unsupported; use development or public", c.DeploymentMode)
	}
	if c.maxConcurrentSessionsErr != nil {
		return c.maxConcurrentSessionsErr
	}
	if c.databaseDedicatedErr != nil {
		return c.databaseDedicatedErr
	}
	if c.secretLoadErr != nil {
		return c.secretLoadErr
	}
	if c.MaxConcurrentSessions < 0 {
		return fmt.Errorf("MAX_CONCURRENT_SESSIONS must be zero (development only) or a positive integer")
	}
	if strings.TrimSpace(c.ProblemsRepoPath) == "" {
		return fmt.Errorf("PROBLEMS_REPO_PATH is required")
	}
	if strings.TrimSpace(c.ProblemArtifactStorePath) == "" {
		return fmt.Errorf("PROBLEM_ARTIFACT_STORE_PATH is required")
	}
	if err := validateDisjointPaths(c.ProblemsRepoPath, c.ProblemArtifactStorePath); err != nil {
		return err
	}
	if c.DeploymentMode == "public" && c.MaxConcurrentSessions <= 0 {
		return fmt.Errorf("MAX_CONCURRENT_SESSIONS must be a positive integer in public mode")
	}
	migrationMode := c.DatabaseMigrationMode
	if migrationMode == "" && c.DeploymentMode == "development" {
		migrationMode = "auto"
	}
	switch migrationMode {
	case "auto":
		if c.DeploymentMode == "public" {
			return fmt.Errorf("DATABASE_MIGRATION_MODE=auto is forbidden in public mode; run the one-shot migrator and use external")
		}
	case "external":
	default:
		return fmt.Errorf("DATABASE_MIGRATION_MODE=%q is unsupported; use auto or external", c.DatabaseMigrationMode)
	}
	securityConfigured := c.DatabaseSecurityConfigured()
	if c.DeploymentMode == "public" && !c.hasAnyDatabaseSecuritySetting() {
		return fmt.Errorf("the complete Control Plane PostgreSQL role topology and DATABASE_DEDICATED_CLUSTER=true are required in public mode")
	}
	if securityConfigured || c.hasAnyDatabaseSecuritySetting() {
		if !c.DatabaseDedicatedCluster {
			return fmt.Errorf("DATABASE_DEDICATED_CLUSTER=true is required for PostgreSQL role hardening")
		}
		roles := []struct {
			name  string
			value string
		}{
			{name: "DATABASE_CATALOG_OWNER_ROLE", value: c.DatabaseCatalogOwnerRole},
			{name: "DATABASE_OWNER_ROLE", value: c.DatabaseOwnerRole},
			{name: "DATABASE_MIGRATOR_ROLE", value: c.DatabaseMigratorRole},
			{name: "DATABASE_RUNTIME_ROLE", value: c.DatabaseRuntimeRole},
			{name: "DATABASE_VALIDATOR_ROLE", value: c.DatabaseValidatorRole},
		}
		seen := make(map[string]string, len(roles))
		for _, role := range roles {
			if !postgresRolePattern.MatchString(role.value) {
				return fmt.Errorf("%s must be a canonical PostgreSQL role identifier", role.name)
			}
			if previous, duplicate := seen[role.value]; duplicate {
				return fmt.Errorf("%s and %s must name distinct PostgreSQL roles", previous, role.name)
			}
			seen[role.value] = role.name
		}
		databaseURL, err := url.Parse(c.DatabaseURL)
		if err != nil || databaseURL.User == nil || databaseURL.User.Username() != c.DatabaseRuntimeRole {
			return fmt.Errorf("DATABASE_URL must authenticate as DATABASE_RUNTIME_ROLE")
		}
	}
	if c.DeploymentMode == "public" {
		if err := c.validatePublicDeployment(); err != nil {
			return err
		}
	}
	switch c.RunnerProvider {
	case "local-docker":
		if c.DeploymentMode != "development" || !local {
			return fmt.Errorf("RUNNER_PROVIDER=%q requires DEPLOYMENT_MODE=development and a loopback FRONTEND_URL", c.RunnerProvider)
		}
		if !isDevelopmentBind(c.ServerBindMode, c.ServerHost) {
			return fmt.Errorf("RUNNER_PROVIDER=%q requires SERVER_BIND_MODE=loopback with a numeric loopback host, or SERVER_BIND_MODE=compose-private with SERVER_HOST=backend", c.RunnerProvider)
		}
		if strings.TrimSpace(c.RunnerScope) == "" {
			return fmt.Errorf("RUNNER_SCOPE is required for scoped local Docker ownership")
		}
		if c.RunnerScope != strings.TrimSpace(c.RunnerScope) {
			return fmt.Errorf("RUNNER_SCOPE must be canonical and contain no leading or trailing whitespace")
		}
	case "home-proxmox", "cloud":
		return fmt.Errorf("RUNNER_PROVIDER=%q is not implemented in this build; refusing to fall back to local Docker", c.RunnerProvider)
	default:
		return fmt.Errorf("RUNNER_PROVIDER=%q is unsupported", c.RunnerProvider)
	}
	if c.PoolSize > 0 {
		return fmt.Errorf("POOL_SIZE=%d is incompatible with the generation-bound Runner seam; set POOL_SIZE=0", c.PoolSize)
	}
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

func (c *Config) validatePublicDeployment() error {
	if !canonicalPublicOrigin(c.FrontendURL) {
		return fmt.Errorf("FRONTEND_URL must be a canonical HTTPS DNS origin without credentials, path, query, or fragment in public mode")
	}
	if strings.TrimSpace(c.GithubClientID) == "" || c.GithubClientID == "your-github-oauth-client-id" {
		return fmt.Errorf("GITHUB_CLIENT_ID must be a non-placeholder OAuth client id in public mode")
	}
	if weakSecret(c.GithubClientSecret) || c.GithubClientSecret == "your-github-oauth-client-secret" {
		return fmt.Errorf("GITHUB_CLIENT_SECRET must be a strong non-placeholder secret in public mode")
	}
	if !c.databaseURLFromFile || !c.githubSecretFromFile || !c.jwtSecretFromFile || !c.jwtRefreshFromFile {
		return fmt.Errorf("public mode requires DATABASE_URL_FILE, GITHUB_CLIENT_SECRET_FILE, JWT_SECRET_FILE, and JWT_REFRESH_SECRET_FILE")
	}
	if c.LLMAPIKey != "" && !c.llmAPIKeyFromFile {
		return fmt.Errorf("public mode requires LLM_API_KEY_FILE when LLM grading is configured")
	}
	if !databaseUsesVerifiedTLS(c.DatabaseURL) {
		return fmt.Errorf("DATABASE_URL must use sslmode=verify-full in public mode")
	}
	if !canonicalHTTPSServiceURL(c.LLMBaseURL) {
		return fmt.Errorf("LLM_BASE_URL must be a canonical HTTPS URL in public mode")
	}
	return nil
}

func canonicalPublicOrigin(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Host == "" ||
		parsed.Path != "" || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.Fragment != "" ||
		parsed.Opaque != "" || parsed.Host != strings.ToLower(parsed.Host) || parsed.String() != raw {
		return false
	}
	host := parsed.Hostname()
	return net.ParseIP(host) == nil && validDNSHostname(host)
}

func canonicalHTTPSServiceURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Host == "" ||
		parsed.RawPath != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" ||
		parsed.Host != strings.ToLower(parsed.Host) || parsed.String() != raw || strings.HasSuffix(parsed.Path, "/") {
		return false
	}
	return net.ParseIP(parsed.Hostname()) == nil && validDNSHostname(parsed.Hostname())
}

func validDNSHostname(host string) bool {
	if host == "" || len(host) > 253 || !strings.Contains(host, ".") || strings.HasSuffix(host, ".") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if !((char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '-') {
				return false
			}
		}
	}
	return true
}

func databaseUsesVerifiedTLS(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "postgres" && parsed.Scheme != "postgresql" || parsed.Hostname() == "" {
		return false
	}
	values, found := parsed.Query()["sslmode"]
	return found && len(values) == 1 && values[0] == "verify-full"
}

var postgresRolePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// DatabaseSecurityConfigured reports whether the server has the complete,
// non-secret role topology required to attest a least-privilege runtime pool.
// Migrator and validator credentials are intentionally not part of Config.
func (c *Config) DatabaseSecurityConfigured() bool {
	if c == nil || !c.DatabaseDedicatedCluster {
		return false
	}
	return c.DatabaseCatalogOwnerRole != "" && c.DatabaseOwnerRole != "" &&
		c.DatabaseMigratorRole != "" && c.DatabaseRuntimeRole != "" &&
		c.DatabaseValidatorRole != ""
}

func (c *Config) hasAnyDatabaseSecuritySetting() bool {
	if c == nil {
		return false
	}
	return c.DatabaseDedicatedCluster || c.DatabaseCatalogOwnerRole != "" ||
		c.DatabaseOwnerRole != "" || c.DatabaseMigratorRole != "" ||
		c.DatabaseRuntimeRole != "" || c.DatabaseValidatorRole != ""
}

// The mutable authoring checkout and immutable runtime CAS must never share a
// directory tree. A checkout replacement or cleanup must not be able to remove
// artifacts already referenced by PostgreSQL.
func validateDisjointPaths(problemsPath, artifactPath string) error {
	problemsAbs, err := filepath.Abs(filepath.Clean(problemsPath))
	if err != nil {
		return fmt.Errorf("resolve PROBLEMS_REPO_PATH: %w", err)
	}
	artifactAbs, err := filepath.Abs(filepath.Clean(artifactPath))
	if err != nil {
		return fmt.Errorf("resolve PROBLEM_ARTIFACT_STORE_PATH: %w", err)
	}
	contains := func(parent, child string) bool {
		rel, err := filepath.Rel(parent, child)
		return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	if problemsAbs == artifactAbs || contains(problemsAbs, artifactAbs) || contains(artifactAbs, problemsAbs) {
		return fmt.Errorf("PROBLEM_ARTIFACT_STORE_PATH and PROBLEMS_REPO_PATH must be disjoint directory trees")
	}
	return nil
}

func isDevelopmentBind(mode, host string) bool {
	mode = strings.ToLower(strings.TrimSpace(mode))
	host = strings.ToLower(strings.TrimSpace(host))
	switch mode {
	case "loopback":
		return host == "127.0.0.1" || host == "::1"
	case "compose-private":
		return host == "backend"
	default:
		return false
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvOrSecretFile(valueKey, fileKey, fallback string) (string, bool, error) {
	value := os.Getenv(valueKey)
	path := os.Getenv(fileKey)
	if value != "" && path != "" {
		return "", false, fmt.Errorf("%s and %s are mutually exclusive", valueKey, fileKey)
	}
	if path == "" {
		if value != "" {
			return value, false, nil
		}
		return fallback, false, nil
	}
	secret, err := readPrivateSecretFile(path)
	if err != nil {
		return "", false, fmt.Errorf("load %s: %w", fileKey, err)
	}
	return secret, true, nil
}

func readPrivateSecretFile(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", errors.New("inspect secret file")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("secret file must be regular and accessible only by its owner")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", errors.New("open secret file")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() || opened.Mode().Perm()&0o077 != 0 {
		return "", errors.New("secret file changed or is not private")
	}
	stat, ok := opened.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return "", errors.New("secret file must be owned by the current user")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxSecretFileBytes+1))
	if err != nil {
		return "", errors.New("read secret file")
	}
	if len(data) == 0 || len(data) > maxSecretFileBytes {
		return "", errors.New("secret file is empty or too large")
	}
	value := strings.TrimSpace(string(data))
	if value == "" || strings.ContainsAny(value, "\r\n") {
		return "", errors.New("secret file must contain exactly one value")
	}
	return value, nil
}

func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func getEnvIntStrict(key string, fallback int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s=%q must be an integer", key, v)
	}
	return n, nil
}

func getEnvBoolStrict(key string, fallback bool) (bool, error) {
	v := os.Getenv(key)
	if v == "" {
		return fallback, nil
	}
	value, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s=%q must be true or false", key, v)
	}
	return value, nil
}
