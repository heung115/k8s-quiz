package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestServer(content string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := chatResponse{
			Choices: []struct {
				Message chatMessage `json:"message"`
			}{{Message: chatMessage{Role: "assistant", Content: content}}},
		}
		json.NewEncoder(w).Encode(resp)
	}))
}

func TestNewClientDefaults(t *testing.T) {
	c := NewClient("", "", "")
	if c.baseURL != "https://api.openai.com/v1" {
		t.Errorf("expected default base URL, got %s", c.baseURL)
	}
	if c.model != "gpt-4o-mini" {
		t.Errorf("expected default model, got %s", c.model)
	}
}

func TestGradeNoAPIKey(t *testing.T) {
	c := NewClient("", "", "")
	_, _, err := c.Grade(context.Background(), "rubric", "evidence")
	if err == nil {
		t.Fatal("expected error for missing API key")
	}
}

func TestGradePass(t *testing.T) {
	server := newTestServer("PASS: problem solved")
	defer server.Close()

	c := NewClient("test-key", "test-model", server.URL)
	success, log, err := c.Grade(context.Background(), "rubric", "evidence")
	if err != nil {
		t.Fatalf("Grade failed: %v", err)
	}
	if !success {
		t.Error("expected success for PASS response")
	}
	if log == "" {
		t.Error("expected non-empty log")
	}
}

func TestGradeFail(t *testing.T) {
	server := newTestServer("FAIL: not solved yet")
	defer server.Close()

	c := NewClient("test-key", "test-model", server.URL)
	success, _, err := c.Grade(context.Background(), "rubric", "evidence")
	if err != nil {
		t.Fatalf("Grade failed: %v", err)
	}
	if success {
		t.Error("expected failure for FAIL response")
	}
}

func TestGradeAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"bad key"}`))
	}))
	defer server.Close()

	c := NewClient("bad-key", "test-model", server.URL)
	_, _, err := c.Grade(context.Background(), "rubric", "evidence")
	if err == nil {
		t.Fatal("expected error for API error response")
	}
}
