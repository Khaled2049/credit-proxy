package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// OllamaProvider calls a locally running Ollama server.
//
// Required env vars: none (runs locally by default)
// Optional env vars:
//   OLLAMA_BASE_URL  (default: http://localhost:11434)
//   OLLAMA_MODEL     (default: llama3)
type OllamaProvider struct {
	baseURL string
	model   string
	client  *http.Client
}

func (o *OllamaProvider) Name() string { return "ollama/" + o.model }

func (o *OllamaProvider) Generate(ctx context.Context, opts GenerateOpts) (GenerateResult, error) {
	body := map[string]any{
		"model":  o.model,
		"prompt": opts.Prompt,
		"stream": false,
		"options": map[string]any{
			"num_predict": opts.MaxOutputTokens,
			"temperature": opts.Temperature,
		},
	}
	b, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/api/generate", bytes.NewReader(b))
	if err != nil {
		return GenerateResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.client.Do(req)
	if err != nil {
		return GenerateResult{}, fmt.Errorf("ollama unreachable: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return GenerateResult{}, fmt.Errorf("ollama status %d: %s", resp.StatusCode, string(msg))
	}

	var gr struct {
		Response string `json:"response"`
		Model    string `json:"model"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&gr); err != nil {
		return GenerateResult{}, fmt.Errorf("ollama decode: %w", err)
	}
	return GenerateResult{Output: gr.Response, Model: gr.Model}, nil
}
