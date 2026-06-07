package llmexec

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

// openaiChatRequest is the request body for the
// POST /v1/chat/completions endpoint that every OpenAI-compatible
// provider exposes (OpenAI, DeepSeek, Anthropic-via-gateway, ollama,
// vLLM, llama.cpp's --server, …). We deliberately do NOT depend on
// openai-go or any vendor SDK — the wire format is small and stable,
// and the surface area we need is a 4-field system/user + assistant
// turn flow. The fields below match the OpenAI v1 reference; most
// providers accept this verbatim or with a single `model` rename.
type openaiChatRequest struct {
	Model    string             `json:"model"`
	Messages []openaiChatMessage `json:"messages"`
	// Stream is explicitly false. The worker reads a single
	// non-streaming response; streaming a token-by-token LLM reply
	// through the task queue is a follow-up — it would also need
	// Realtime message backfill plumbing.
	Stream bool `json:"stream"`
}

type openaiChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// openaiChatResponse is the subset of the v1 response we care about.
// We deliberately do not unmarshal `usage` (would force every
// provider to populate it identically) — the task usage is recorded
// in the task call / usage report path, not here.
type openaiChatResponse struct {
	Choices []struct {
		Message openaiChatMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error,omitempty"`
}

// ErrUpstream is returned by Do when the LLM provider responded with a
// non-2xx status. The body is included so callers can log it without
// us needing to invent a typed error hierarchy. network-level
// failures (DNS, dial, TLS) are returned as plain errors from net/http.
type ErrUpstream struct {
	StatusCode int
	Body       string
	Provider   string
}

func (e *ErrUpstream) Error() string {
	body := e.Body
	if len(body) > 256 {
		body = body[:256] + "…"
	}
	return fmt.Sprintf("llmexec: %s returned %d: %s", e.Provider, e.StatusCode, body)
}

// OpenAIClient is a minimal OpenAI-compatible HTTP client. It is safe
// for concurrent use; the only mutable state is the per-call timeout
// applied via context.WithTimeout. Construct one per Worker and reuse
// it across calls.
type OpenAIClient struct {
	HTTPClient *http.Client
	// ExtraHeaders, when non-nil, are merged into every outbound
	// request. The LLM provider handlers do not use this — we leave
	// the hook for org-specific deployments that need to inject a
	// custom Authorization scheme or tracing header.
	ExtraHeaders http.Header
}

// NewOpenAIClient returns a client with a sane default timeout. The
// transport pool is the stdlib default, which is fine for the
// outbound-LLM-call workload: providers are usually called over a
// small number of long-lived HTTP/2 connections.
func NewOpenAIClient() *OpenAIClient {
	return &OpenAIClient{
		HTTPClient: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
}

// Ping issues a lightweight GET to {baseURL}/models to verify that
// the OpenAI-compatible endpoint is reachable. Returns nil on any
// 2xx; non-2xx and network errors are surfaced verbatim. Used by the
// worker keep-alive pass (server/internal/llmexec.worker) to bump
// the runtime's last_seen_at and to flip a previously-offline
// runtime back to online once the URL is reachable again.
//
// 8 MiB cap on the response body mirrors Do: the /models response
// from a busy local Ollama/LM Studio instance can be sizable.
func (c *OpenAIClient) Ping(ctx context.Context, baseURL, apiKey string) error {
	url := strings.TrimRight(baseURL, "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("llmexec: build ping: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	for k, vs := range c.ExtraHeaders {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("llmexec: ping http: %w", err)
	}
	defer resp.Body.Close()
	const maxRead = 8 << 20
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxRead))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &ErrUpstream{
			StatusCode: resp.StatusCode,
			Provider:   baseURL,
			Body:       "ping: non-2xx from /models",
		}
	}
	return nil
}

// Do sends a single chat-completion request to the supplied base URL
// and returns the assistant turn. The supplied model is sent
// verbatim; we do NOT prepend any vendor-specific prefix (no
// "openai/" for anthropic, no "accounts/fireworks/models/" for
// fireworks, …) because the workspace operator configured that
// model name into the provider row already.
//
// The supplied context controls both cancellation and timeout — a
// short context (e.g. per-task deadline) truncates the upstream call
// cleanly.
func (c *OpenAIClient) Do(ctx context.Context, baseURL, model, apiKey, systemPrompt, userPrompt string) (string, error) {
	baseURL = strings.TrimRight(baseURL, "/")
	url := baseURL + "/chat/completions"
	body, err := json.Marshal(openaiChatRequest{
		Model: model,
		Messages: []openaiChatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userPrompt},
		},
		Stream: false,
	})
	if err != nil {
		return "", fmt.Errorf("llmexec: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("llmexec: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	for k, vs := range c.ExtraHeaders {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("llmexec: http do: %w", err)
	}
	defer resp.Body.Close()
	// 8 MiB cap; we only need the assistant text, but some providers
	// (notably ollama with --verbose) return large debug payloads.
	const maxRead = 8 << 20
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxRead))
	if err != nil {
		return "", fmt.Errorf("llmexec: read body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", &ErrUpstream{
			StatusCode: resp.StatusCode,
			Body:       string(raw),
			Provider:   baseURL,
		}
	}
	var parsed openaiChatResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("llmexec: unmarshal response: %w", err)
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		// Some providers return 200 + a structured error envelope
		// (e.g. OpenAI on rate limit). Surface as upstream error so
		// the task is failed, not silently retried.
		return "", &ErrUpstream{
			StatusCode: 200,
			Body:       parsed.Error.Message,
			Provider:   baseURL,
		}
	}
	if len(parsed.Choices) == 0 {
		return "", errors.New("llmexec: response had no choices")
	}
	return parsed.Choices[0].Message.Content, nil
}
