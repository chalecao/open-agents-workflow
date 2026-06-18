package llmexec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// clientLogger is the package-level slog used by OpenAIClient. It
// defaults to slog.Default() (the server's main logger) so warnings
// emitted from inside Ping / Chat end up in the same stream as the
// worker's other logs. Tests can replace it via SetClientLogger to
// silence the output; production code should leave it alone.
var clientLogger = slog.Default()

// SetClientLogger overrides the slog used by OpenAIClient. Intended
// for tests that need to silence the "sending unauthenticated
// request" warning; production code should rely on the default.
func SetClientLogger(l *slog.Logger) {
	clientLogger = l
}

// Tool describes a single function the model can invoke, in the
// OpenAI v1 /chat/completions "tools" array shape. Parameters is a
// JSON Schema object (the same one that documents the function on
// the OpenAI side) and must be a non-nil RawMessage — pass
// json.RawMessage(`{"type":"object","properties":{}}`) for a no-arg
// function, not a nil slice, otherwise the model receives the
// invalid "{}" zero value and most providers reject the request.
type Tool struct {
	Type     string       `json:"type"` // always "function" today
	Function ToolFunction `json:"function"`
}

// ToolFunction is the function body of a Tool.
type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ToolCall is a single model-requested tool invocation as returned
// on the assistant message. The function arguments arrive as a
// JSON-encoded string (not a parsed object) — parse them at the
// dispatch site with json.Unmarshal into a typed args struct so
// the worker fails fast on a malformed call instead of silently
// passing the raw string downstream.
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"` // "function"
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// ChatMessage is the wire format for one entry in the messages
// array. The four OpenAI roles we use map as follows:
//   - "system":    only the opening system prompt (Content set)
//   - "user":      the original task prompt (Content set)
//   - "assistant": model output (Content and/or ToolCalls set)
//   - "tool":      one tool result per assistant tool_call
//     (ToolCallID + Content set; the model uses the
//     id to pair the result back to its call)
type ChatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// chatRequest is the body posted to /chat/completions. Tools is
// omitted when nil so the legacy "plain text" request shape is
// preserved bit-for-bit — Do() relies on that for its tests.
type chatRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`
	Tools    []Tool        `json:"tools,omitempty"`
	Stream   bool          `json:"stream"`
}

// chatResponse is the subset of the v1 response we care about.
// We deliberately do not unmarshal `usage` (would force every
// provider to populate it identically) — the task usage is
// recorded in the task call / usage report path, not here.
type chatResponse struct {
	Choices []struct {
		Message ChatMessage `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error,omitempty"`
}

// ErrUpstream is returned by Chat when the LLM provider responded
// with a non-2xx status (or a 200 + structured error envelope).
// The body is included so callers can log it without us needing
// to invent a typed error hierarchy. network-level failures (DNS,
// dial, TLS) are returned as plain errors from net/http.
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

// OpenAIClient is a minimal OpenAI-compatible HTTP client. It is
// safe for concurrent use; the only mutable state is the per-call
// timeout applied via context.WithTimeout. Construct one per
// Worker and reuse it across calls.
type OpenAIClient struct {
	HTTPClient *http.Client
	// ExtraHeaders, when non-nil, are merged into every outbound
	// request. The LLM provider handlers do not use this — we leave
	// the hook for org-specific deployments that need to inject a
	// custom Authorization scheme or tracing header.
	ExtraHeaders http.Header
}

// NewOpenAIClient returns a client with a sane default timeout.
// The transport pool is the stdlib default, which is fine for
// the outbound-LLM-call workload: providers are usually called
// over a small number of long-lived HTTP/2 connections.
func NewOpenAIClient() *OpenAIClient {
	return &OpenAIClient{
		HTTPClient: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
}

// Ping issues a lightweight GET to {baseURL}/models to verify that
// the OpenAI-compatible endpoint is reachable. Returns nil on any
// 2xx; non-2xx and network errors are surfaced verbatim. Used by
// the worker keep-alive pass (server/internal/llmexec.worker) to
// bump the runtime's last_seen_at and to flip a previously-offline
// runtime back to online once the URL is reachable again.
//
// 8 MiB cap on the response body mirrors Chat: the /models
// response from a busy local Ollama/LM Studio instance can be
// sizable.
func (c *OpenAIClient) Ping(ctx context.Context, baseURL, apiKey string) error {
	url := strings.TrimRight(baseURL, "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("llmexec: build ping: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	// Deliberately log when we are NOT sending Authorization so a 401
	// in the log makes it obvious whether the request was sent
	// unauthenticated by us (empty apiKey, e.g. a local Ollama /
	// LM Studio / vLLM endpoint) or the operator forgot to put a key
	// in the provider row. Without this log line the only signal is
	// the upstream 401 body, which is the kind of silent-misconfig
	// that produced MUL-XXXX.
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	} else {
		clientLogger.Warn("llmexec: sending unauthenticated request (apiKey empty; this is fine for local Ollama / LM Studio / vLLM, but a 401 on a hosted provider means the provider row has no API key)",
			"base_url", baseURL,
			"endpoint", url,
		)
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

// Do is the legacy "single-turn plain text" entry point. It
// returns just the assistant's content string and is preserved
// (with the same wire shape) for the worker's pre-tool era and
// for the existing tests in openai_client_test.go. New code
// should call Chat directly.
func (c *OpenAIClient) Do(ctx context.Context, baseURL, model, apiKey, systemPrompt, userPrompt string) (string, error) {
	msg, err := c.Chat(ctx, baseURL, model, apiKey, []ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userPrompt},
	}, nil)
	if err != nil {
		return "", err
	}
	return msg.Content, nil
}

// Chat makes a single /chat/completions request. The caller owns
// the messages slice and is expected to keep appending assistant /
// tool turns to it across calls — the worker drives that loop in
// executeTask. When tools is non-nil it is sent verbatim; a
// function-calling-capable model can populate the response
// message's ToolCalls field, which the caller dispatches and
// feeds back as role:"tool" messages.
//
// The supplied context controls both cancellation and timeout —
// a short context (e.g. per-task deadline) truncates the upstream
// call cleanly.
func (c *OpenAIClient) Chat(ctx context.Context, baseURL, model, apiKey string, messages []ChatMessage, tools []Tool) (ChatMessage, error) {
	baseURL = strings.TrimRight(baseURL, "/")
	url := baseURL + "/chat/completions"
	body, err := json.Marshal(chatRequest{
		Model:    model,
		Messages: messages,
		Tools:    tools,
		Stream:   false,
	})
	if err != nil {
		return ChatMessage{}, fmt.Errorf("llmexec: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return ChatMessage{}, fmt.Errorf("llmexec: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	// Deliberately log when we are NOT sending Authorization so a 401
	// in the log makes it obvious whether the request was sent
	// unauthenticated by us (empty apiKey, e.g. a local Ollama /
	// LM Studio / vLLM endpoint) or the operator forgot to put a key
	// in the provider row. Without this log line the only signal is
	// the upstream 401 body, which is the kind of silent-misconfig
	// that produced MUL-XXXX.
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	} else {
		clientLogger.Warn("llmexec: sending unauthenticated request (apiKey empty; this is fine for local Ollama / LM Studio / vLLM, but a 401 on a hosted provider means the provider row has no API key)",
			"base_url", baseURL,
			"endpoint", url,
		)
	}
	for k, vs := range c.ExtraHeaders {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return ChatMessage{}, fmt.Errorf("llmexec: http do: %w", err)
	}
	defer resp.Body.Close()
	// 8 MiB cap; we only need the assistant text + tool calls, but
	// some providers (notably ollama with --verbose) return large
	// debug payloads.
	const maxRead = 8 << 20
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxRead))
	if err != nil {
		return ChatMessage{}, fmt.Errorf("llmexec: read body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ChatMessage{}, &ErrUpstream{
			StatusCode: resp.StatusCode,
			Body:       string(raw),
			Provider:   baseURL,
		}
	}
	var parsed chatResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return ChatMessage{}, fmt.Errorf("llmexec: unmarshal response: %w", err)
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		// Some providers return 200 + a structured error envelope
		// (e.g. OpenAI on rate limit). Surface as upstream error
		// so the task is failed, not silently retried.
		return ChatMessage{}, &ErrUpstream{
			StatusCode: 200,
			Body:       parsed.Error.Message,
			Provider:   baseURL,
		}
	}
	if len(parsed.Choices) == 0 {
		return ChatMessage{}, errors.New("llmexec: response had no choices")
	}
	return parsed.Choices[0].Message, nil
}
