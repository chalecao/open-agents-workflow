package handler

import "testing"

// TestValidateLLMBaseURL is a security boundary test. The function gates
// every LLM provider create/update at the handler layer: a malicious or
// accidental input that produces a non-absolute http(s) URL must be
// rejected, otherwise the LLM execution worker would happily dial an
// attacker-controlled target inside the server's network. The cases
// below cover the protocol allowlist, host requirement, the
// no-credentials rule, and the trailing-slash normalisation the
// downstream concatenations rely on.
func TestValidateLLMBaseURL(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "https endpoint with path", input: "https://api.openai.com/v1", want: "https://api.openai.com/v1"},
		{name: "http endpoint", input: "http://localhost:11434/v1", want: "http://localhost:11434/v1"},
		{name: "trims trailing slash", input: "https://api.openai.com/v1/", want: "https://api.openai.com/v1"},
		{name: "trims multiple trailing slashes", input: "https://api.openai.com/v1///", want: "https://api.openai.com/v1"},
		{name: "preserves internal-routable host", input: "http://10.0.0.5:8080/openai/v1/", want: "http://10.0.0.5:8080/openai/v1"},

		{name: "empty", input: "", wantErr: true},
		{name: "whitespace only", input: "   ", wantErr: true},
		{name: "missing scheme", input: "api.openai.com/v1", wantErr: true},
		{name: "file scheme rejected", input: "file:///etc/passwd", wantErr: true},
		{name: "javascript scheme rejected", input: "javascript:alert(1)", wantErr: true},
		{name: "ftp scheme rejected", input: "ftp://example.com", wantErr: true},
		{name: "missing host", input: "https://", wantErr: true},
		{name: "embedded userinfo rejected", input: "https://user:pass@api.openai.com/v1", wantErr: true},
		{name: "embedded user-only rejected", input: "https://user@api.openai.com/v1", wantErr: true},
		{name: "relative path rejected", input: "/api/openai/v1", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := validateLLMBaseURL(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (output=%q)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSealLLMAPIKey documents the nil-cipher contract: a nil box with
// a non-empty key is an operator misconfiguration (no
// MULTICA_LLM_SECRET_KEY set in .env). Surfacing that as a 400 keeps a
// half-configured server from logging the plaintext instead.
func TestSealLLMAPIKey(t *testing.T) {
	if got, err := sealLLMAPIKey(nil, ""); err != nil || got != nil {
		t.Fatalf("empty key with nil box: got (%v, %v), want (nil, nil)", got, err)
	}
	if got, err := sealLLMAPIKey(nil, "sk-1234"); err == nil {
		t.Fatalf("non-empty key with nil box: expected error, got nil (ciphertext=%v)", got)
	}
}
