package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// AnthropicProvider calls the Anthropic Messages API (Claude models).
//
// Required env vars: ANTHROPIC_API_KEY
// Optional env vars:
//   ANTHROPIC_MODEL    (default: claude-sonnet-4-6)
//   ANTHROPIC_BASE_URL (default: https://api.anthropic.com/v1)
type AnthropicProvider struct {
	apiKey  string
	model   string
	baseURL string
	client  *http.Client
}

func (a *AnthropicProvider) Name() string { return "anthropic/" + a.model }

func (a *AnthropicProvider) Generate(ctx context.Context, opts GenerateOpts) (GenerateResult, error) {
	body := map[string]any{
		"model":      a.model,
		"max_tokens": opts.MaxOutputTokens,
		"messages": []map[string]string{
			{"role": "user", "content": opts.Prompt},
		},
	}
	b, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/messages", bytes.NewReader(b))
	if err != nil {
		return GenerateResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", a.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := a.client.Do(req)
	if err != nil {
		return GenerateResult{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return GenerateResult{}, &ProviderError{Provider: "anthropic", StatusCode: resp.StatusCode, Message: string(msg)}
	}

	var gr struct {
		Model   string `json:"model"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&gr); err != nil {
		return GenerateResult{}, fmt.Errorf("anthropic decode: %w", err)
	}
	if len(gr.Content) == 0 {
		return GenerateResult{}, fmt.Errorf("anthropic returned no content")
	}
	return GenerateResult{Output: gr.Content[0].Text, Model: gr.Model}, nil
}
