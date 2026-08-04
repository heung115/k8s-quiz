package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsLocal(t *testing.T) {
	cases := map[string]bool{
		"http://localhost:5173":     true,
		"http://127.0.0.1:8080":     true,
		"http://[::1]:3000":         true,
		"https://localhost":         true,
		"":                          false, // SEC3-1: fail closed on empty
		"prod.example.com":          false, // scheme-less must NOT be local
		"https://k8squiz.io":        false,
		"https://quiz.example.com":  false,
		"ftp://localhost":           false, // non-http(s) scheme
		"http://localhost.evil.com": false,
	}
	for url, want := range cases {
		c := &Config{FrontendURL: url}
		if got := c.IsLocal(); got != want {
			t.Errorf("IsLocal(%q) = %v, want %v", url, got, want)
		}
	}
}

func TestCookieSecure(t *testing.T) {
	if (&Config{FrontendURL: "http://localhost:5173"}).CookieSecure() {
		t.Error("http frontend must not set Secure cookies")
	}
	if !(&Config{FrontendURL: "https://quiz.example.com"}).CookieSecure() {
		t.Error("https frontend must set Secure cookies")
	}
}

func TestLoadMaxConcurrentSessions(t *testing.T) {
	t.Setenv("MAX_CONCURRENT_SESSIONS", "7")
	if got := Load().MaxConcurrentSessions; got != 7 {
		t.Errorf("expected 7, got %d", got)
	}
	t.Setenv("MAX_CONCURRENT_SESSIONS", "")
	if got := Load().MaxConcurrentSessions; got != 0 {
		t.Errorf("development default must be 0 (unlimited), got %d", got)
	}
}

func TestValidateMaxConcurrentSessionsFailsClosed(t *testing.T) {
	t.Run("malformed", func(t *testing.T) {
		t.Setenv("MAX_CONCURRENT_SESSIONS", "ten")
		cfg := Load()
		cfg.FrontendURL = "http://localhost:5173"
		cfg.ServerBindMode = "loopback"
		cfg.ServerHost = "127.0.0.1"
		cfg.RunnerScope = "test-scope"
		if err := cfg.Validate(); err == nil {
			t.Fatal("malformed capacity must fail startup")
		}
	})

	t.Run("negative development", func(t *testing.T) {
		cfg := strongConfig("http://localhost:5173", "local-docker")
		cfg.MaxConcurrentSessions = -1
		if err := cfg.Validate(); err == nil {
			t.Fatal("negative capacity must fail startup")
		}
	})

	for _, capacity := range []int{0, -1} {
		t.Run(fmt.Sprintf("public %d", capacity), func(t *testing.T) {
			cfg := strongConfig("https://quiz.example.com", "home-proxmox")
			cfg.DeploymentMode = "public"
			cfg.MaxConcurrentSessions = capacity
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), "MAX_CONCURRENT_SESSIONS") {
				t.Fatalf("public capacity error = %v", err)
			}
		})
	}
}

func strongConfig(frontendURL, provider string) *Config {
	return &Config{
		DeploymentMode:           "development",
		ServerBindMode:           "loopback",
		ServerHost:               "127.0.0.1",
		FrontendURL:              frontendURL,
		RunnerProvider:           provider,
		RunnerScope:              "test-scope",
		ProblemsRepoPath:         "/tmp/k8s-quiz/problems",
		ProblemArtifactStorePath: "/tmp/k8s-quiz/artifacts",
		JWTSecret:                "strong-jwt-secret-value",
		JWTRefreshSecret:         "strong-refresh-secret-value",
	}
}

func TestLoadRunnerProvider(t *testing.T) {
	t.Setenv("RUNNER_PROVIDER", "")
	if got := Load().RunnerProvider; got != "local-docker" {
		t.Fatalf("default provider = %q, want local-docker", got)
	}
	t.Setenv("RUNNER_PROVIDER", "home-proxmox")
	if got := Load().RunnerProvider; got != "home-proxmox" {
		t.Fatalf("configured provider = %q, want home-proxmox", got)
	}
}

func TestLoadProblemArtifactStorePath(t *testing.T) {
	t.Setenv("PROBLEM_ARTIFACT_STORE_PATH", "")
	if got := Load().ProblemArtifactStorePath; got != "./data/problem-artifacts" {
		t.Fatalf("default artifact store path = %q", got)
	}
	t.Setenv("PROBLEM_ARTIFACT_STORE_PATH", "/srv/k8s-quiz/artifacts")
	if got := Load().ProblemArtifactStorePath; got != "/srv/k8s-quiz/artifacts" {
		t.Fatalf("configured artifact store path = %q", got)
	}
}

func configureDatabaseSecurity(cfg *Config) {
	cfg.DatabaseCatalogOwnerRole = "kq_pg_catalog_owner"
	cfg.DatabaseOwnerRole = "kq_cp_owner"
	cfg.DatabaseMigratorRole = "kq_cp_migrator"
	cfg.DatabaseRuntimeRole = "kq_cp_runtime"
	cfg.DatabaseValidatorRole = "kq_cp_validator"
	cfg.DatabaseDedicatedCluster = true
}

func TestValidatePublicDatabaseBoundary(t *testing.T) {
	base := func() *Config {
		cfg := strongConfig("https://quiz.example.com", "home-proxmox")
		cfg.DeploymentMode = "public"
		cfg.MaxConcurrentSessions = 1
		cfg.DatabaseURL = "postgres://kq_cp_runtime:secret@db.example/k8squiz?sslmode=require"
		configureDatabaseSecurity(cfg)
		return cfg
	}

	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{name: "automatic migration", mutate: func(c *Config) { c.DatabaseMigrationMode = "auto" }, want: "forbidden in public mode"},
		{name: "missing runtime role", mutate: func(c *Config) { c.DatabaseMigrationMode = "external"; c.DatabaseRuntimeRole = "" }, want: "DATABASE_RUNTIME_ROLE must be"},
		{name: "role mismatch", mutate: func(c *Config) { c.DatabaseMigrationMode = "external"; c.DatabaseRuntimeRole = "other_runtime" }, want: "must authenticate as"},
		{name: "invalid role", mutate: func(c *Config) { c.DatabaseMigrationMode = "external"; c.DatabaseRuntimeRole = "Runtime;SET ROLE" }, want: "canonical PostgreSQL role"},
		{name: "shared cluster", mutate: func(c *Config) { c.DatabaseMigrationMode = "external"; c.DatabaseDedicatedCluster = false }, want: "DATABASE_DEDICATED_CLUSTER=true"},
		{name: "duplicate roles", mutate: func(c *Config) { c.DatabaseMigrationMode = "external"; c.DatabaseValidatorRole = c.DatabaseRuntimeRole }, want: "distinct PostgreSQL roles"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := base()
			test.mutate(cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate error=%v, want %q", err, test.want)
			}
		})
	}
}

func TestLoadDatabaseSecurityTopology(t *testing.T) {
	t.Setenv("DATABASE_CATALOG_OWNER_ROLE", "catalog_owner")
	t.Setenv("DATABASE_OWNER_ROLE", "control_owner")
	t.Setenv("DATABASE_MIGRATOR_ROLE", "control_migrator")
	t.Setenv("DATABASE_RUNTIME_ROLE", "control_runtime")
	t.Setenv("DATABASE_VALIDATOR_ROLE", "control_validator")
	t.Setenv("DATABASE_DEDICATED_CLUSTER", "true")
	cfg := Load()
	if !cfg.DatabaseSecurityConfigured() {
		t.Fatal("complete database security topology was not loaded")
	}
	if cfg.DatabaseCatalogOwnerRole != "catalog_owner" || cfg.DatabaseValidatorRole != "control_validator" {
		t.Fatalf("database roles were not loaded: %+v", cfg)
	}
}

func TestValidateRejectsMalformedDedicatedClusterFlag(t *testing.T) {
	t.Setenv("DATABASE_DEDICATED_CLUSTER", "yes-please")
	cfg := Load()
	cfg.FrontendURL = "http://localhost:5173"
	cfg.ServerBindMode = "loopback"
	cfg.ServerHost = "127.0.0.1"
	cfg.RunnerScope = "test-scope"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "must be true or false") {
		t.Fatalf("malformed dedicated-cluster confirmation error = %v", err)
	}
}

func TestValidateRequiresDisjointProblemAndArtifactPaths(t *testing.T) {
	tests := []struct {
		name, problems, artifacts string
		wantErr                   bool
	}{
		{name: "siblings", problems: "/srv/k8s-quiz/problems", artifacts: "/srv/k8s-quiz/artifacts"},
		{name: "same", problems: "/srv/k8s-quiz/problems", artifacts: "/srv/k8s-quiz/problems", wantErr: true},
		{name: "artifact inside checkout", problems: "/srv/k8s-quiz/problems", artifacts: "/srv/k8s-quiz/problems/.artifacts", wantErr: true},
		{name: "checkout inside artifact root", problems: "/srv/k8s-quiz/runtime/problems", artifacts: "/srv/k8s-quiz/runtime", wantErr: true},
		{name: "blank artifact", problems: "/srv/k8s-quiz/problems", artifacts: " \t ", wantErr: true},
		{name: "blank problems", problems: "", artifacts: "/srv/k8s-quiz/artifacts", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := strongConfig("http://localhost:5173", "local-docker")
			cfg.ProblemsRepoPath = test.problems
			cfg.ProblemArtifactStorePath = test.artifacts
			err := cfg.Validate()
			if (err != nil) != test.wantErr {
				t.Fatalf("Validate() error=%v wantErr=%v", err, test.wantErr)
			}
		})
	}
}

func TestValidateRunnerProviderFailClosed(t *testing.T) {
	tests := []struct {
		name     string
		frontend string
		provider string
		wantErr  bool
	}{
		{"local development", "http://localhost:5173", "local-docker", false},
		{"public local docker", "https://quiz.example.com", "local-docker", true},
		{"scheme-less is public", "quiz.example.com", "local-docker", true},
		{"lookalike hostname", "http://localhost.evil.com", "local-docker", true},
		{"proxmox not implemented", "https://quiz.example.com", "home-proxmox", true},
		{"cloud not implemented", "https://quiz.example.com", "cloud", true},
		{"unknown", "http://localhost:5173", "docker", true},
		{"empty literal", "http://localhost:5173", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := strongConfig(tc.frontend, tc.provider).Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() error = %v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestValidateLocalDockerRequiresExplicitDevelopmentAndSafeBind(t *testing.T) {
	tests := []struct {
		name, mode, host string
		wantErr          bool
	}{
		{"loopback", "development", "127.0.0.1", false},
		{"ipv6 loopback", "development", "::1", false},
		{"compose hostname without mode", "development", "backend", true},
		{"public mode", "public", "127.0.0.1", true},
		{"all interfaces", "development", "0.0.0.0", true},
		{"empty host", "development", "", true},
		{"unknown mode", "staging", "127.0.0.1", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := strongConfig("http://localhost:5173", "local-docker")
			cfg.DeploymentMode, cfg.ServerHost = tc.mode, tc.host
			err := cfg.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() error=%v, wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

func TestValidateComposePrivateBindRequiresExactModeAndHost(t *testing.T) {
	cfg := strongConfig("http://localhost:5173", "local-docker")
	cfg.ServerBindMode = "compose-private"
	cfg.ServerHost = "backend"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("explicit Compose private bind rejected: %v", err)
	}
	cfg.ServerHost = "0.0.0.0"
	if err := cfg.Validate(); err == nil {
		t.Fatal("compose-private must not permit an arbitrary bind host")
	}
	cfg.ServerHost = "backend"
	cfg.ServerBindMode = "loopback"
	if err := cfg.Validate(); err == nil {
		t.Fatal("backend hostname must not be accepted in ordinary loopback mode")
	}
}

func TestValidateRejectsLegacyWarmPool(t *testing.T) {
	cfg := strongConfig("http://localhost:5173", "local-docker")
	cfg.PoolSize = 1
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected generation-unsafe warm pool to be rejected")
	}
}

func TestValidateRejectsEmptyRunnerScope(t *testing.T) {
	cfg := strongConfig("http://localhost:5173", "local-docker")
	cfg.RunnerScope = " \t "
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected blank RUNNER_SCOPE to be rejected")
	}
}

func TestValidateRejectsNonCanonicalRunnerScope(t *testing.T) {
	for _, scope := range []string{" test-scope", "test-scope ", "\ttest-scope"} {
		cfg := strongConfig("http://localhost:5173", "local-docker")
		cfg.RunnerScope = scope
		if err := cfg.Validate(); err == nil {
			t.Fatalf("expected noncanonical RUNNER_SCOPE %q to be rejected", scope)
		}
	}
}

func validPublicDeploymentConfig() *Config {
	return &Config{
		DeploymentMode:       "public",
		FrontendURL:          "https://quiz.example.com",
		DatabaseURL:          "postgres://kq_cp_runtime:secret@db.example.com/k8squiz?sslmode=verify-full",
		GithubClientID:       "github-client-id",
		GithubClientSecret:   "strong-github-client-secret",
		JWTSecret:            "strong-jwt-secret-value",
		JWTRefreshSecret:     "strong-refresh-secret-value",
		LLMBaseURL:           "https://api.example.com/v1",
		databaseURLFromFile:  true,
		githubSecretFromFile: true,
		jwtSecretFromFile:    true,
		jwtRefreshFromFile:   true,
	}
}

func TestValidatePublicDeploymentRequiresCanonicalTLSAndFileSecrets(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{name: "valid", mutate: func(*Config) {}},
		{name: "http origin", mutate: func(c *Config) { c.FrontendURL = "http://quiz.example.com" }, want: "canonical HTTPS"},
		{name: "origin path", mutate: func(c *Config) { c.FrontendURL = "https://quiz.example.com/app" }, want: "canonical HTTPS"},
		{name: "origin query", mutate: func(c *Config) { c.FrontendURL = "https://quiz.example.com?tenant=x" }, want: "canonical HTTPS"},
		{name: "origin userinfo", mutate: func(c *Config) { c.FrontendURL = "https://user@quiz.example.com" }, want: "canonical HTTPS"},
		{name: "IP origin", mutate: func(c *Config) { c.FrontendURL = "https://192.0.2.1" }, want: "canonical HTTPS"},
		{name: "placeholder client id", mutate: func(c *Config) { c.GithubClientID = "your-github-oauth-client-id" }, want: "GITHUB_CLIENT_ID"},
		{name: "placeholder client secret", mutate: func(c *Config) { c.GithubClientSecret = "your-github-oauth-client-secret" }, want: "GITHUB_CLIENT_SECRET"},
		{name: "database environment", mutate: func(c *Config) { c.databaseURLFromFile = false }, want: "DATABASE_URL_FILE"},
		{name: "github environment", mutate: func(c *Config) { c.githubSecretFromFile = false }, want: "GITHUB_CLIENT_SECRET_FILE"},
		{name: "jwt environment", mutate: func(c *Config) { c.jwtSecretFromFile = false }, want: "JWT_SECRET_FILE"},
		{name: "database TLS disabled", mutate: func(c *Config) {
			c.DatabaseURL = "postgres://kq_cp_runtime:secret@db.example.com/k8squiz?sslmode=disable"
		}, want: "verify-full"},
		{name: "database TLS require", mutate: func(c *Config) {
			c.DatabaseURL = "postgres://kq_cp_runtime:secret@db.example.com/k8squiz?sslmode=require"
		}, want: "verify-full"},
		{name: "http LLM", mutate: func(c *Config) { c.LLMBaseURL = "http://api.example.com/v1" }, want: "LLM_BASE_URL"},
		{name: "LLM query", mutate: func(c *Config) { c.LLMBaseURL = "https://api.example.com/v1?token=x" }, want: "LLM_BASE_URL"},
		{name: "LLM key environment", mutate: func(c *Config) { c.LLMAPIKey = "configured-key" }, want: "LLM_API_KEY_FILE"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := validPublicDeploymentConfig()
			test.mutate(cfg)
			err := cfg.validatePublicDeployment()
			if test.want == "" && err != nil {
				t.Fatalf("valid public deployment rejected: %v", err)
			}
			if test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)) {
				t.Fatalf("error=%v, want %q", err, test.want)
			}
		})
	}
}

func writeSecretFile(t *testing.T, name, value string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(value), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadSecretFileContract(t *testing.T) {
	t.Run("private regular file", func(t *testing.T) {
		path := writeSecretFile(t, "jwt", "secret-value\n", 0o600)
		t.Setenv("JWT_SECRET", "")
		t.Setenv("JWT_SECRET_FILE", path)
		cfg := Load()
		if cfg.secretLoadErr != nil || cfg.JWTSecret != "secret-value" || !cfg.jwtSecretFromFile {
			t.Fatalf("loaded secret=%q fromFile=%t err=%v", cfg.JWTSecret, cfg.jwtSecretFromFile, cfg.secretLoadErr)
		}
	})

	t.Run("environment and file conflict", func(t *testing.T) {
		path := writeSecretFile(t, "jwt", "secret-value", 0o600)
		t.Setenv("JWT_SECRET", "environment-secret")
		t.Setenv("JWT_SECRET_FILE", path)
		if err := Load().Validate(); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
			t.Fatalf("conflicting secret error=%v", err)
		}
	})

	tests := []struct {
		name  string
		setup func(*testing.T) string
	}{
		{name: "world readable", setup: func(t *testing.T) string { return writeSecretFile(t, "secret", "SENSITIVE_MARKER", 0o644) }},
		{name: "multiline", setup: func(t *testing.T) string { return writeSecretFile(t, "secret", "SENSITIVE_MARKER\ntwo", 0o600) }},
		{name: "oversize", setup: func(t *testing.T) string {
			return writeSecretFile(t, "secret", strings.Repeat("x", maxSecretFileBytes+1), 0o600)
		}},
		{name: "symlink", setup: func(t *testing.T) string {
			target := writeSecretFile(t, "target", "SENSITIVE_MARKER", 0o600)
			link := filepath.Join(t.TempDir(), "link")
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			return link
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("JWT_SECRET", "")
			t.Setenv("JWT_SECRET_FILE", test.setup(t))
			if err := Load().Validate(); err == nil || strings.Contains(err.Error(), "SENSITIVE_MARKER") {
				t.Fatalf("unsafe secret error=%v", err)
			}
		})
	}
}
