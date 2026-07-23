package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type Grader interface {
	Grade(ctx context.Context, rubric, evidence string) (bool, string, error)
}

type Client struct {
	apiKey  string
	model   string
	baseURL string
	http    *http.Client
}

func NewClient(apiKey, model, baseURL string) *Client {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	if model == "" {
		model = "gpt-4o-mini"
	}
	return &Client{
		apiKey:  apiKey,
		model:   model,
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 60 * time.Second},
	}
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
}

func (c *Client) Grade(ctx context.Context, rubric, evidence string) (bool, string, error) {
	if c.apiKey == "" {
		return false, "", errors.New("LLM API key not configured")
	}

	systemPrompt := "You are grading a Kubernetes troubleshooting exercise. " +
		"Given the grading rubric and the current cluster state, decide whether the problem is solved. " +
		"Respond with a single line starting with \"PASS:\" or \"FAIL:\" followed by a brief reason."

	userPrompt := "Grading rubric:\n" + rubric + "\n\nCurrent cluster state:\n" + evidence

	reqBody := chatRequest{
		Model: c.model,
		Messages: []chatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return false, "", err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return false, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return false, "", err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, "", err
	}
	if resp.StatusCode != http.StatusOK {
		return false, "", fmt.Errorf("LLM API error: %d %s", resp.StatusCode, string(respBody))
	}

	var parsed chatResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return false, "", err
	}
	if len(parsed.Choices) == 0 {
		return false, "", errors.New("LLM returned no choices")
	}

	answer := strings.TrimSpace(parsed.Choices[0].Message.Content)
	success := strings.HasPrefix(strings.ToUpper(answer), "PASS")
	return success, answer, nil
}
