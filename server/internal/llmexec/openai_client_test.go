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
	var req openaiChatRequest
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
