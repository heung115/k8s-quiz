package config

import "testing"

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
		t.Errorf("default must be 0 (unlimited), got %d", got)
	}
}
