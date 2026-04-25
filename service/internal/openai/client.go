// Package openai provides a thin net/http wrapper around the OpenAI Chat
// Completions API. No SDK dependency is added — same design as internal/ifdb.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client wraps the OpenAI API. All fields are unexported; use NewClient.
type Client struct {
	httpClient *http.Client
	apiKey     string
	baseURL    string
	model      string
}

// ClientOptions configures a Client.
type ClientOptions struct {
	APIKey     string
	BaseURL    string        // default: https://api.openai.com/v1
	Model      string        // default: gpt-5.2
	Timeout    time.Duration // default: 120s (non-stream); stream uses a separate long-lived client
	HTTPClient *http.Client  // injectable for tests
}

// NewClient creates a new Client.
func NewClient(opts ClientOptions) *Client {
	if opts.BaseURL == "" {
		opts.BaseURL = "https://api.openai.com/v1"
	}
	if opts.Model == "" {
		opts.Model = "gpt-5.2"
	}
	if opts.Timeout == 0 {
		opts.Timeout = 120 * time.Second
	}
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: opts.Timeout}
	}
	return &Client{
		httpClient: httpClient,
		apiKey:     opts.APIKey,
		baseURL:    opts.BaseURL,
		model:      opts.Model,
	}
}

// Message is a single chat message.
type Message struct {
	Role    string `json:"role"`    // "system" | "user" | "assistant"
	Content string `json:"content"`
}

// ChatRequest is the body sent to the Chat Completions endpoint.
type ChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Stream      bool      `json:"stream,omitempty"`
	Temperature float64   `json:"temperature,omitempty"`
}

// ChatResponse holds the result of a synchronous Chat call.
type ChatResponse struct {
	Model            string
	Content          string
	PromptTokens     int
	CompletionTokens int
	FinishReason     string
	Err              error
}

// Chat sends a synchronous (non-streaming) chat completion request.
func (c *Client) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	req.Stream = false
	body, err := json.Marshal(req)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return ChatResponse{}, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("http do: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return ChatResponse{}, fmt.Errorf("openai status %d: %s", resp.StatusCode, raw)
	}

	var apiResp struct {
		Model   string `json:"model"`
		Choices []struct {
			Message      Message `json:"message"`
			FinishReason string  `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return ChatResponse{}, fmt.Errorf("decode response: %w", err)
	}

	var content, finishReason string
	if len(apiResp.Choices) > 0 {
		content = apiResp.Choices[0].Message.Content
		finishReason = apiResp.Choices[0].FinishReason
	}
	return ChatResponse{
		Model:            apiResp.Model,
		Content:          content,
		PromptTokens:     apiResp.Usage.PromptTokens,
		CompletionTokens: apiResp.Usage.CompletionTokens,
		FinishReason:     finishReason,
	}, nil
}
