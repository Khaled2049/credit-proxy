package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func fixedResponseClient(status int, body string) *http.Client {
	return &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: status,
				Body:       io.NopCloser(strings.NewReader(body)),
				Header:     make(http.Header),
			}, nil
		}),
	}
}

func TestClassifyProviderStatus(t *testing.T) {
	cases := []struct {
		name           string
		providerStatus int
		want           int
	}{
		{"unauthorized", 401, 401},
		{"forbidden treated as auth failure", 403, 401},
		{"rate limited", 429, 429},
		{"model not found", 404, 404},
		{"bad request", 400, 400},
		{"internal server error is unclassified", 500, 502},
		{"bad gateway is unclassified", 502, 502},
		{"service unavailable is unclassified", 503, 502},
		{"network failure has no status code", 0, 502},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyProviderStatus(tc.providerStatus); got != tc.want {
				t.Errorf("classifyProviderStatus(%d) = %d, want %d", tc.providerStatus, got, tc.want)
			}
		})
	}
}

func TestProviderError_Error(t *testing.T) {
	withStatus := &ProviderError{Provider: "gemini", StatusCode: 401, Message: "bad key"}
	if got, want := withStatus.Error(), "gemini status 401: bad key"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}

	networkFailure := &ProviderError{Provider: "ollama", Message: "unreachable: dial tcp: connection refused"}
	if got, want := networkFailure.Error(), "ollama: unreachable: dial tcp: connection refused"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestGeminiProvider_ReturnsProviderErrorOnAuthFailure(t *testing.T) {
	g := &GeminiProvider{
		apiKey: "bad-key",
		model:  "gemini-2.5-flash-lite",
		client: fixedResponseClient(401, `{"error":{"message":"API key not valid"}}`),
	}
	_, err := g.Generate(context.Background(), GenerateOpts{Prompt: "hi", MaxOutputTokens: 16})
	assertProviderError(t, err, "gemini", 401)
}

func TestOpenAIProvider_ReturnsProviderErrorOnRateLimit(t *testing.T) {
	o := &OpenAIProvider{
		apiKey:  "k",
		model:   "gpt-4o-mini",
		baseURL: "https://example.invalid",
		client:  fixedResponseClient(429, `{"error":"rate limited"}`),
	}
	_, err := o.Generate(context.Background(), GenerateOpts{Prompt: "hi", MaxOutputTokens: 16})
	assertProviderError(t, err, "openai", 429)
}

func TestAnthropicProvider_ReturnsProviderErrorOnNotFound(t *testing.T) {
	a := &AnthropicProvider{
		apiKey:  "k",
		model:   "claude-nonexistent",
		baseURL: "https://example.invalid",
		client:  fixedResponseClient(404, `{"error":"model not found"}`),
	}
	_, err := a.Generate(context.Background(), GenerateOpts{Prompt: "hi", MaxOutputTokens: 16})
	assertProviderError(t, err, "anthropic", 404)
}

func TestOllamaProvider_ReturnsProviderErrorOnServerError(t *testing.T) {
	o := &OllamaProvider{
		baseURL: "http://example.invalid",
		model:   "phi4-mini",
		client:  fixedResponseClient(500, "internal error"),
	}
	_, err := o.Generate(context.Background(), GenerateOpts{Prompt: "hi", MaxOutputTokens: 16})
	assertProviderError(t, err, "ollama", 500)
}

func TestOllamaProvider_NetworkFailureIsProviderErrorWithZeroStatus(t *testing.T) {
	o := &OllamaProvider{
		baseURL: "http://127.0.0.1:1", // nothing listens here
		model:   "phi4-mini",
		client:  &http.Client{},
	}
	_, err := o.Generate(context.Background(), GenerateOpts{Prompt: "hi", MaxOutputTokens: 16})
	var perr *ProviderError
	if !errors.As(err, &perr) {
		t.Fatalf("expected *ProviderError, got %T: %v", err, err)
	}
	if perr.StatusCode != 0 {
		t.Errorf("StatusCode = %d, want 0 (no HTTP response)", perr.StatusCode)
	}
}

func assertProviderError(t *testing.T, err error, wantProvider string, wantStatus int) {
	t.Helper()
	var perr *ProviderError
	if !errors.As(err, &perr) {
		t.Fatalf("expected *ProviderError, got %T: %v", err, err)
	}
	if perr.Provider != wantProvider {
		t.Errorf("Provider = %q, want %q", perr.Provider, wantProvider)
	}
	if perr.StatusCode != wantStatus {
		t.Errorf("StatusCode = %d, want %d", perr.StatusCode, wantStatus)
	}
}
