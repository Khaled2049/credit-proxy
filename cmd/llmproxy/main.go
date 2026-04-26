package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/kh1011/creditproxy/pkg/contracts"
	"github.com/kh1011/creditproxy/pkg/httpx"
	"github.com/kh1011/creditproxy/pkg/tokens"
	"github.com/kh1011/creditproxy/pkg/version"
)

type server struct {
	apiKey    string
	model     string
	useMock   bool
	http      *http.Client
	userAgent string
}

func main() {
	addr := getenv("LLMPROXY_ADDR", ":8082")
	s := &server{
		apiKey:    os.Getenv("GEMINI_API_KEY"),
		model:     getenv("GEMINI_MODEL", "gemini-2.0-flash"),
		useMock:   strings.EqualFold(getenv("LLM_MOCK_MODE", "true"), "true"),
		http:      httpx.NewHTTPClient(30),
		userAgent: "creditproxy-llmproxy/1.0",
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/v1/generate", s.handleGenerate)

	log.Printf("llmproxy listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"version": version.Version,
		"commit":  version.Commit,
	})
}

func (s *server) handleGenerate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req contracts.GenerateRequest
	if err := httpx.ReadJSON(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Prompt == "" {
		http.Error(w, "prompt is required", http.StatusBadRequest)
		return
	}
	if req.MaxOutputTokens <= 0 {
		req.MaxOutputTokens = 256
	}
	if req.Temperature == 0 {
		req.Temperature = 0.7
	}

	var output string
	var err error
	model := s.model
	useMock := req.ForceMock || s.useMock || s.apiKey == ""
	if useMock {
		model = "mock-gemini"
		output = fmt.Sprintf("Mock response to: %s", req.Prompt)
	} else {
		output, err = s.callGemini(r, req.Prompt, req.MaxOutputTokens, req.Temperature)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
	}

	promptTokens := tokens.Estimate(req.Prompt)
	completionTokens := tokens.Estimate(output)
	httpx.WriteJSON(w, http.StatusOK, contracts.GenerateResponse{
		Output: output,
		Model:  model,
		Usage: contracts.GenerateUsage{
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      promptTokens + completionTokens,
		},
	})
}

func (s *server) callGemini(r *http.Request, prompt string, maxOutputTokens int64, temperature float64) (string, error) {
	endpoint := "https://generativelanguage.googleapis.com/v1beta/models/" + url.PathEscape(s.model) + ":generateContent?key=" + url.QueryEscape(s.apiKey)
	body := map[string]any{
		"contents": []map[string]any{
			{"parts": []map[string]string{{"text": prompt}}},
		},
		"generationConfig": map[string]any{
			"maxOutputTokens": maxOutputTokens,
			"temperature":     temperature,
		},
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint, bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", s.userAgent)
	resp, err := s.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("gemini status %d: %s", resp.StatusCode, string(msg))
	}
	var gr struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&gr); err != nil {
		return "", err
	}
	if len(gr.Candidates) == 0 || len(gr.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("gemini returned no candidates")
	}
	return gr.Candidates[0].Content.Parts[0].Text, nil
}

func getenv(key, fallback string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	return v
}
