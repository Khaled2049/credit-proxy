package main

import (
	"crypto/subtle"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/kh1011/creditproxy/pkg/contracts"
	"github.com/kh1011/creditproxy/pkg/httpx"
	"github.com/kh1011/creditproxy/pkg/ids"
	"github.com/kh1011/creditproxy/pkg/tokens"
	"github.com/kh1011/creditproxy/pkg/version"
)

type server struct {
	provider Provider
	mock     *MockProvider
}

func main() {
	addr := getenv("LLMPROXY_ADDR", ":8082")

	provider, err := newProvider()
	if err != nil {
		log.Fatalf("configure provider: %v", err)
	}
	log.Printf("llmproxy using provider: %s", provider.Name())

	s := &server{
		provider: provider,
		mock:     &MockProvider{},
	}

	internalToken := strings.TrimSpace(os.Getenv("INTERNAL_SERVICE_TOKEN"))

	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/v1/generate", requireInternalToken(internalToken, s.handleGenerate))

	log.Printf("llmproxy listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

// newProvider selects and constructs the configured LLM provider.
//
// Selection order:
//  1. LLM_PROVIDER env var ("gemini", "mock")
//  2. LLM_MOCK_MODE=true → mock
//  3. GEMINI_API_KEY set → gemini
//  4. default → mock
//
// To add a new provider: add a case here and implement the Provider interface.
func newProvider() (Provider, error) {
	explicit := strings.ToLower(strings.TrimSpace(os.Getenv("LLM_PROVIDER")))
	mockMode := strings.EqualFold(getenv("LLM_MOCK_MODE", "true"), "true")
	apiKey := strings.TrimSpace(os.Getenv("GEMINI_API_KEY"))

	name := explicit
	if name == "" {
		switch {
		case mockMode || apiKey == "":
			name = "mock"
		default:
			name = "gemini"
		}
	}

	switch name {
	case "mock":
		return &MockProvider{}, nil

	case "gemini":
		if apiKey == "" {
			return nil, fmt.Errorf("LLM_PROVIDER=gemini requires GEMINI_API_KEY")
		}
		return &GeminiProvider{
			apiKey:    apiKey,
			model:     getenv("GEMINI_MODEL", "gemini-2.0-flash"),
			client:    httpx.NewHTTPClient(time.Duration(30) * time.Second),
			userAgent: "creditproxy-llmproxy/" + version.Version,
		}, nil

	case "openai":
		if strings.TrimSpace(os.Getenv("OPENAI_API_KEY")) == "" {
			return nil, fmt.Errorf("LLM_PROVIDER=openai requires OPENAI_API_KEY")
		}
		return &OpenAIProvider{
			apiKey:  strings.TrimSpace(os.Getenv("OPENAI_API_KEY")),
			model:   getenv("OPENAI_MODEL", "gpt-4o-mini"),
			baseURL: getenv("OPENAI_BASE_URL", "https://api.openai.com/v1"),
			client:  httpx.NewHTTPClient(time.Duration(30) * time.Second),
		}, nil

	case "anthropic":
		if strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")) == "" {
			return nil, fmt.Errorf("LLM_PROVIDER=anthropic requires ANTHROPIC_API_KEY")
		}
		return &AnthropicProvider{
			apiKey:  strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")),
			model:   getenv("ANTHROPIC_MODEL", "claude-sonnet-4-6"),
			baseURL: getenv("ANTHROPIC_BASE_URL", "https://api.anthropic.com/v1"),
			client:  httpx.NewHTTPClient(time.Duration(30) * time.Second),
		}, nil

	case "ollama":
		return &OllamaProvider{
			baseURL: getenv("OLLAMA_BASE_URL", "http://localhost:11434"),
			model:   getenv("OLLAMA_MODEL", "phi4-mini:latest"),
			client:  httpx.NewHTTPClient(time.Duration(120) * time.Second),
		}, nil

	default:
		return nil, fmt.Errorf("unknown LLM_PROVIDER %q — supported: gemini, openai, anthropic, ollama, mock", name)
	}
}

// newProviderFromBYOK constructs a per-request Provider from BYOK fields.
// Returns (nil, nil) when no BYOK fields are set (caller should use default provider).
func newProviderFromBYOK(req contracts.GenerateRequest) (Provider, error) {
	if req.BYOKProvider == "" || req.BYOKApiKey == "" {
		return nil, nil
	}
	client := httpx.NewHTTPClient(time.Duration(30) * time.Second)
	switch strings.ToLower(req.BYOKProvider) {
	case "gemini":
		model := req.BYOKModel
		if model == "" {
			model = "gemini-2.5-flash"
		}
		return &GeminiProvider{
			apiKey:    req.BYOKApiKey,
			model:     model,
			client:    client,
			userAgent: "creditproxy-llmproxy/" + version.Version,
		}, nil
	case "openai":
		model := req.BYOKModel
		if model == "" {
			model = "gpt-4o-mini"
		}
		return &OpenAIProvider{
			apiKey:  req.BYOKApiKey,
			model:   model,
			baseURL: "https://api.openai.com/v1",
			client:  client,
		}, nil
	case "anthropic", "claude":
		model := req.BYOKModel
		if model == "" {
			model = "claude-haiku-4-5-20251001"
		}
		return &AnthropicProvider{
			apiKey:  req.BYOKApiKey,
			model:   model,
			baseURL: "https://api.anthropic.com/v1",
			client:  client,
		}, nil
	default:
		return nil, fmt.Errorf("unknown byok_provider %q — supported: gemini, openai, anthropic", req.BYOKProvider)
	}
}

func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]string{
		"status":   "ok",
		"version":  version.Version,
		"commit":   version.Commit,
		"provider": s.provider.Name(),
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

	p := s.provider
	if req.ForceMock {
		p = s.mock
	} else if byok, err2 := newProviderFromBYOK(req); err2 == nil && byok != nil {
		p = byok
	} else if err2 != nil {
		http.Error(w, "invalid byok config: "+err2.Error(), http.StatusBadRequest)
		return
	}

	result, err := p.Generate(r.Context(), GenerateOpts{
		Prompt:          req.Prompt,
		MaxOutputTokens: req.MaxOutputTokens,
		Temperature:     req.Temperature,
	})
	if err != nil {
		// Provider errors can embed upstream response bodies; log them but
		// return only a generic message + correlation ref to the caller.
		reqID := ids.New("llm")
		log.Printf("provider %s generate failed ref=%s: %v", p.Name(), reqID, err)
		http.Error(w, "generation failed (ref "+reqID+")", http.StatusBadGateway)
		return
	}

	promptTokens := tokens.Estimate(req.Prompt)
	completionTokens := tokens.Estimate(result.Output)
	httpx.WriteJSON(w, http.StatusOK, contracts.GenerateResponse{
		Output: result.Output,
		Model:  result.Model,
		Usage: contracts.GenerateUsage{
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      promptTokens + completionTokens,
		},
	})
}

func requireInternalToken(token string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if token != "" {
			got := strings.TrimSpace(r.Header.Get("X-Internal-Token"))
			if got == "" {
				http.Error(w, "missing internal token", http.StatusUnauthorized)
				return
			}
			if subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
				http.Error(w, "invalid internal token", http.StatusUnauthorized)
				return
			}
		}
		next(w, r)
	}
}

func getenv(key, fallback string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	return v
}
