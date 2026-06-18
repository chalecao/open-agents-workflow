package llmexec

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOpenAIClient_Do_Success(t *testing.T) {
	calls := 0
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		gotAuth = r.Header.Get("Authorization")
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"hello back"}}]}`))
	}))
	defer srv.Close()

	c := NewOpenAIClient()
	out, err := c.Do(context.Background(), srv.URL, "gpt-x", "secret-key", "you are X", "hi")
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if out != "hello back" {
		t.Fatalf("assistant content: got %q", out)
	}
	if calls != 1 {
		t.Fatalf("calls=%d, want 1", calls)
	}
	if gotAuth != "Bearer secret-key" {
		t.Fatalf("auth header: got %q", gotAuth)
	}
	// body sanity
	var req chatRequest
	if err := json.Unmarshal([]byte(gotBody), &req); err != nil {
		t.Fatalf("request body parse: %v", err)
	}
	if req.Model != "gpt-x" || len(req.Messages) != 2 {
		t.Fatalf("unexpected request: %+v", req)
	}
	if req.Messages[0].Role != "system" || req.Messages[0].Content != "you are X" {
		t.Fatalf("system message: %+v", req.Messages[0])
	}
	if req.Messages[1].Role != "user" || req.Messages[1].Content != "hi" {
		t.Fatalf("user message: %+v", req.Messages[1])
	}
	if req.Stream {
		t.Fatalf("Stream must be false")
	}
}

func TestOpenAIClient_Do_NoAuthWhenEmpty(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()
	c := NewOpenAIClient()
	if _, err := c.Do(context.Background(), srv.URL, "m", "", "sys", "user"); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if gotAuth != "" {
		t.Fatalf("expected no Authorization header for empty api key, got %q", gotAuth)
	}
}

func TestOpenAIClient_Do_UpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited","type":"rate_limit_error"}}`))
	}))
	defer srv.Close()
	c := NewOpenAIClient()
	_, err := c.Do(context.Background(), srv.URL, "m", "k", "sys", "u")
	if err == nil {
		t.Fatalf("expected error on 429")
	}
	var upErr *ErrUpstream
	if !asErr(err, &upErr) {
		t.Fatalf("expected *ErrUpstream, got %T", err)
	}
	if upErr.StatusCode != 429 {
		t.Fatalf("status: got %d", upErr.StatusCode)
	}
	if !strings.Contains(upErr.Body, "rate limited") {
		t.Fatalf("body should include upstream message, got %q", upErr.Body)
	}
}

func TestOpenAIClient_Do_StripsTrailingSlash(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()
	c := NewOpenAIClient()
	if _, err := c.Do(context.Background(), srv.URL+"/", "m", "k", "sys", "u"); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if gotPath != "/chat/completions" {
		t.Fatalf("path: got %q", gotPath)
	}
}

// asErr is a tiny helper to keep the assertion readable without
// pulling in errors.As at the test site.
func asErr(err error, target **ErrUpstream) bool {
	if err == nil {
		return false
	}
	if up, ok := err.(*ErrUpstream); ok {
		*target = up
		return true
	}
	return false
}

func TestOpenAIClient_Ping_Success(t *testing.T) {
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer srv.Close()

	c := NewOpenAIClient()
	if err := c.Ping(context.Background(), srv.URL, "secret-key"); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if gotPath != "/models" {
		t.Fatalf("path: got %q, want /models", gotPath)
	}
	if gotAuth != "Bearer secret-key" {
		t.Fatalf("auth header: got %q, want Bearer secret-key", gotAuth)
	}
}

func TestOpenAIClient_Ping_StripsTrailingSlash(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := NewOpenAIClient()
	if err := c.Ping(context.Background(), srv.URL+"/", ""); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if gotPath != "/models" {
		t.Fatalf("path: got %q, want /models (no double slash)", gotPath)
	}
}

func TestOpenAIClient_Ping_Non2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
	}))
	defer srv.Close()
	c := NewOpenAIClient()
	err := c.Ping(context.Background(), srv.URL, "")
	if err == nil {
		t.Fatalf("expected error on 401")
	}
	var upErr *ErrUpstream
	if !asErr(err, &upErr) {
		t.Fatalf("expected *ErrUpstream, got %T", err)
	}
	if upErr.StatusCode != 401 {
		t.Fatalf("status: got %d, want 401", upErr.StatusCode)
	}
}

// TestOpenAIClient_Chat_WithTools exercises the multi-turn tool
// path: a request that includes a tools array must hit the wire
// intact, and the response's tool_calls (including the JSON-encoded
// arguments string) must round-trip back to the caller. This is
// the contract the worker's executeTask loop depends on.
func TestOpenAIClient_Chat_WithTools(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 8192)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices":[{
				"message":{
					"role":"assistant",
					"content":"",
					"tool_calls":[
						{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"README.md\"}"}}
					]
				}
			}]
		}`))
	}))
	defer srv.Close()

	c := NewOpenAIClient()
	tools := []Tool{{
		Type: "function",
		Function: ToolFunction{
			Name:        "read_file",
			Description: "Read a file from the workdir",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
		},
	}}
	msg, err := c.Chat(context.Background(), srv.URL, "m", "k",
		[]ChatMessage{{Role: "user", Content: "read README"}}, tools)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if msg.Content != "" {
		t.Fatalf("expected empty content, got %q", msg.Content)
	}
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(msg.ToolCalls))
	}
	if msg.ToolCalls[0].Function.Name != "read_file" {
		t.Fatalf("tool name: %q", msg.ToolCalls[0].Function.Name)
	}
	if msg.ToolCalls[0].ID != "call_1" {
		t.Fatalf("tool id: %q", msg.ToolCalls[0].ID)
	}
	if msg.ToolCalls[0].Function.Arguments != `{"path":"README.md"}` {
		t.Fatalf("tool args: %q", msg.ToolCalls[0].Function.Arguments)
	}

	// Wire-shape sanity: tools array must be present and non-empty.
	var req chatRequest
	if err := json.Unmarshal([]byte(gotBody), &req); err != nil {
		t.Fatalf("parse body: %v", err)
	}
	if len(req.Tools) != 1 {
		t.Fatalf("tools on wire: got %d, want 1", len(req.Tools))
	}
	if req.Tools[0].Function.Name != "read_file" {
		t.Fatalf("tool wire name: %q", req.Tools[0].Function.Name)
	}
}

// TestOpenAIClient_Chat_OmitToolsWhenNil guarantees the legacy
// "no tools" shape is byte-identical to the pre-tool request —
// Do() callers and any provider that rejects unknown fields both
// rely on the tools key being absent, not an empty array.
func TestOpenAIClient_Chat_OmitToolsWhenNil(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()
	c := NewOpenAIClient()
	if _, err := c.Chat(context.Background(), srv.URL, "m", "k",
		[]ChatMessage{{Role: "user", Content: "hi"}}, nil); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if strings.Contains(gotBody, `"tools"`) {
		t.Fatalf("tools key must be omitted when nil; body=%s", gotBody)
	}
}
