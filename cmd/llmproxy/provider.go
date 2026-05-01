package main

import "context"

// GenerateOpts holds the per-request generation parameters passed to a Provider.
type GenerateOpts struct {
	Prompt          string
	MaxOutputTokens int64
	Temperature     float64
}

// GenerateResult is the normalised response returned by every Provider.
type GenerateResult struct {
	Output string
	Model  string
}

// Provider is the interface every LLM backend must implement.
// Adding a new provider: implement Generate + Name, register it in newProvider().
type Provider interface {
	// Generate calls the underlying LLM and returns the text output.
	Generate(ctx context.Context, opts GenerateOpts) (GenerateResult, error)
	// Name returns a human-readable identifier used in logs and healthz.
	Name() string
}
