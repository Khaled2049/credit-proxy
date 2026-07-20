package main

import (
	"context"
	"fmt"
)

// ProviderError wraps a non-2xx response from an upstream LLM provider API.
// StatusCode is the provider's own HTTP status (0 for non-HTTP failures like
// a network error or a malformed response) — handleGenerate uses it to
// forward the real failure category to the gateway instead of collapsing
// every provider failure into a generic 502.
type ProviderError struct {
	Provider   string
	StatusCode int
	Message    string
}

func (e *ProviderError) Error() string {
	if e.StatusCode == 0 {
		return fmt.Sprintf("%s: %s", e.Provider, e.Message)
	}
	return fmt.Sprintf("%s status %d: %s", e.Provider, e.StatusCode, e.Message)
}

// classifyProviderStatus maps a provider's own HTTP status to the status
// llmproxy returns to the gateway. Only categories the caller can act on
// differently are split out; everything else (5xx, malformed responses,
// StatusCode == 0 for network failures) collapses to 502 — genuinely
// unclassified/transient, safe to treat as "try again".
func classifyProviderStatus(providerStatus int) int {
	switch providerStatus {
	case 401, 403:
		return 401
	case 429:
		return 429
	case 404:
		return 404
	case 400:
		return 400
	default:
		return 502
	}
}

// GenerateOpts holds the per-request generation parameters passed to a Provider.
type GenerateOpts struct {
	Prompt          string
	MaxOutputTokens int64
	Temperature     float64
}

// GenerateResult is the normalised response returned by every Provider.
// PromptTokens/CompletionTokens carry the provider's own reported usage when
// available; HasUsage is false for providers that don't report it (mock, or a
// response missing the usage block), in which case the caller falls back to a
// heuristic estimate of the prompt/output text.
type GenerateResult struct {
	Output           string
	Model            string
	PromptTokens     int64
	CompletionTokens int64
	HasUsage         bool
}

// Provider is the interface every LLM backend must implement.
// Adding a new provider: implement Generate + Name, register it in newProvider().
type Provider interface {
	// Generate calls the underlying LLM and returns the text output.
	Generate(ctx context.Context, opts GenerateOpts) (GenerateResult, error)
	// Name returns a human-readable identifier used in logs and healthz.
	Name() string
}
