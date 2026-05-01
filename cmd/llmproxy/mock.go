package main

import (
	"context"
	"fmt"
)

// MockProvider returns a deterministic canned response. Used when
// LLM_PROVIDER=mock, LLM_MOCK_MODE=true, or no API key is set.
// Also used per-request when force_mock=true is sent by the gateway.
type MockProvider struct{}

func (m *MockProvider) Name() string { return "mock" }

func (m *MockProvider) Generate(_ context.Context, opts GenerateOpts) (GenerateResult, error) {
	return GenerateResult{
		Output: fmt.Sprintf("Mock response to: %s", opts.Prompt),
		Model:  "mock-gemini",
	}, nil
}
