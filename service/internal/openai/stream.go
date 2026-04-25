package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Chunk is one parsed SSE event from an OpenAI streaming response.
// Done is true on the terminal chunk; after it the channel is closed.
type Chunk struct {
	Content string // cumulative concatenation so far
	Delta   string // this chunk's delta text (may be empty on metadata chunks)
	Done    bool
	Err     error

	// Token counts are only populated on the final chunk (requires
	// stream_options.include_usage=true in the request).
	PromptTokens     int
	CompletionTokens int
}

// streamRequest extends ChatRequest with stream_options for usage counts.
type streamRequest struct {
	ChatRequest
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// StreamChat opens an OpenAI SSE stream and returns a channel of Chunks.
// The channel is closed after the Done chunk is emitted. Cancel ctx to abort.
// Callers must drain the channel.
func (c *Client) StreamChat(ctx context.Context, req ChatRequest) (<-chan Chunk, error) {
	req.Stream = true
	sreq := streamRequest{
		ChatRequest:   req,
		StreamOptions: &streamOptions{IncludeUsage: true},
	}
	body, err := json.Marshal(sreq)
	if err != nil {
		return nil, fmt.Errorf("marshal stream request: %w", err)
	}

	// Use a long-lived HTTP client without a hard timeout for streaming.
	streamClient := &http.Client{Timeout: 310 * time.Second}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build stream request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := streamClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http do stream: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close() //nolint:errcheck
		return nil, fmt.Errorf("openai stream status %d: %s", resp.StatusCode, raw)
	}

	ch := make(chan Chunk, 16)
	go func() {
		defer close(ch)
		defer resp.Body.Close() //nolint:errcheck

		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 1<<20), 1<<20) // 1 MiB scanner buffer
		scanner.Split(bufio.ScanLines)

		var cumulative strings.Builder
		var promptTokens, completionTokens int

		for scanner.Scan() {
			line := scanner.Text()

			// SSE keep-alive or comment.
			if line == "" || strings.HasPrefix(line, ":") || strings.HasPrefix(line, "event:") {
				continue
			}

			data, ok := strings.CutPrefix(line, "data: ")
			if !ok {
				continue
			}

			if data == "[DONE]" {
				ch <- Chunk{
					Content:          cumulative.String(),
					Done:             true,
					PromptTokens:     promptTokens,
					CompletionTokens: completionTokens,
				}
				return
			}

			var event struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
					FinishReason *string `json:"finish_reason"`
				} `json:"choices"`
				Usage *struct {
					PromptTokens     int `json:"prompt_tokens"`
					CompletionTokens int `json:"completion_tokens"`
				} `json:"usage"`
			}
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				// Malformed frame — skip silently.
				continue
			}

			// Capture usage when present (last chunk with stream_options.include_usage).
			if event.Usage != nil {
				promptTokens = event.Usage.PromptTokens
				completionTokens = event.Usage.CompletionTokens
			}

			if len(event.Choices) > 0 {
				delta := event.Choices[0].Delta.Content
				if delta != "" {
					cumulative.WriteString(delta)
					ch <- Chunk{
						Content: cumulative.String(),
						Delta:   delta,
					}
				}
			}
		}

		if err := scanner.Err(); err != nil {
			ch <- Chunk{Err: fmt.Errorf("stream scan: %w", err), Done: true}
		}
	}()

	return ch, nil
}
