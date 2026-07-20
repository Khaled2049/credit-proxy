package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// OpenAIProvider calls the OpenAI chat completions API.
// Also works with any OpenAI-compatible API (e.g. local vLLM, Groq, Together AI)
// by setting OPENAI_BASE_URL.
//
// Required env vars: OPENAI_API_KEY
// Optional env vars:
//
//	OPENAI_MODEL     (default: gpt-4o-mini)
//	OPENAI_BASE_URL  (default: https://api.openai.com/v1)
type OpenAIProvider struct {
	apiKey  string
	model   string
	baseURL string
	client  *http.Client
}

func (o *OpenAIProvider) Name() string { return "openai/" + o.model }

func (o *OpenAIProvider) Generate(ctx context.Context, opts GenerateOpts) (GenerateResult, error) {
	body := map[string]any{
		"model": o.model,
		"messages": []map[string]string{
			{"role": "user", "content": opts.Prompt},
		},
		"max_tokens":  opts.MaxOutputTokens,
		"temperature": opts.Temperature,
	}
	b, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/chat/completions", bytes.NewReader(b))
	if err != nil {
		return GenerateResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+o.apiKey)

	resp, err := o.client.Do(req)
	if err != nil {
		return GenerateResult{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return GenerateResult{}, &ProviderError{Provider: "openai", StatusCode: resp.StatusCode, Message: string(msg)}
	}

	var gr struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&gr); err != nil {
		return GenerateResult{}, fmt.Errorf("openai decode: %w", err)
	}
	if len(gr.Choices) == 0 {
		return GenerateResult{}, fmt.Errorf("openai returned no choices")
	}
	return GenerateResult{
		Output:           gr.Choices[0].Message.Content,
		Model:            gr.Model,
		PromptTokens:     gr.Usage.PromptTokens,
		CompletionTokens: gr.Usage.CompletionTokens,
		HasUsage:         gr.Usage.PromptTokens > 0 || gr.Usage.CompletionTokens > 0,
	}, nil
}
