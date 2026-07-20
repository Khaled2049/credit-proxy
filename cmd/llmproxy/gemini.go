package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// GeminiProvider calls the Google Gemini generateContent API.
//
// Required env vars: GEMINI_API_KEY
// Optional env vars: GEMINI_MODEL (default: gemini-2.5-flash-lite)
type GeminiProvider struct {
	apiKey    string
	model     string
	client    *http.Client
	userAgent string
}

func (g *GeminiProvider) Name() string { return "gemini/" + g.model }

func (g *GeminiProvider) Generate(ctx context.Context, opts GenerateOpts) (GenerateResult, error) {
	endpoint := "https://generativelanguage.googleapis.com/v1beta/models/" +
		url.PathEscape(g.model) + ":generateContent"

	body := map[string]any{
		"contents": []map[string]any{
			{"parts": []map[string]string{{"text": opts.Prompt}}},
		},
		"generationConfig": map[string]any{
			"maxOutputTokens": opts.MaxOutputTokens,
			"temperature":     opts.Temperature,
		},
	}
	b, _ := json.Marshal(body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		return GenerateResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", g.userAgent)
	req.Header.Set("x-goog-api-key", g.apiKey)

	resp, err := g.client.Do(req)
	if err != nil {
		return GenerateResult{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return GenerateResult{}, &ProviderError{Provider: "gemini", StatusCode: resp.StatusCode, Message: string(msg)}
	}

	var gr struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
		UsageMetadata struct {
			PromptTokenCount     int64 `json:"promptTokenCount"`
			CandidatesTokenCount int64 `json:"candidatesTokenCount"`
			TotalTokenCount      int64 `json:"totalTokenCount"`
		} `json:"usageMetadata"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&gr); err != nil {
		return GenerateResult{}, err
	}
	if len(gr.Candidates) == 0 || len(gr.Candidates[0].Content.Parts) == 0 {
		return GenerateResult{}, fmt.Errorf("gemini returned no candidates")
	}
	um := gr.UsageMetadata
	return GenerateResult{
		Output:           gr.Candidates[0].Content.Parts[0].Text,
		Model:            g.model,
		PromptTokens:     um.PromptTokenCount,
		CompletionTokens: um.CandidatesTokenCount,
		HasUsage:         um.TotalTokenCount > 0,
	}, nil
}
