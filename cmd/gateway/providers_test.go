package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	authn "github.com/kh1011/creditproxy/pkg/auth"
	"github.com/kh1011/creditproxy/pkg/httpx"
)

func TestHandleProvidersProxiesCuratedCatalog(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/providers" || r.Header.Get("X-Internal-Token") != "internal" {
			http.NotFound(w, r)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"version":   1,
			"providers": []map[string]any{{"id": "gemini", "models": []any{}}},
		})
	}))
	defer upstream.Close()

	s := &server{
		llmURL:        upstream.URL,
		client:        httpx.NewHTTPClient(5 * time.Second),
		internalToken: "internal",
	}
	rr := httptest.NewRecorder()
	s.handleProviders(rr, httptest.NewRequest(http.MethodGet, "/v1/providers", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["version"].(float64) != 1 {
		t.Fatalf("version = %v, want 1", body["version"])
	}
}

func TestHandleProviderValidationForwardsBYOKWithoutReturningKey(t *testing.T) {
	var upstreamKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/providers/validate":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			upstreamKey, _ = body["api_key"].(string)
			httpx.WriteJSON(w, http.StatusOK, map[string]any{
				"valid": true, "provider": "anthropic", "model": "claude-sonnet-4-6",
			})
		case "/v1/events":
			httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "recorded"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	s := &server{
		llmURL:      upstream.URL,
		ledgerURL:   upstream.URL,
		client:      httpx.NewHTTPClient(5 * time.Second),
		verifier:    authn.NewVerifier(authn.Config{Mode: authn.ModeDev}),
		rateLimiter: newUserRateLimiter(10),
	}
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(
		http.MethodPost,
		"/v1/providers/validate",
		strings.NewReader(`{"user_id":"u1","provider":"anthropic","api_key":"super-secret","model":"claude-sonnet-4-6"}`),
	)
	s.handleProviderValidation(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	if upstreamKey != "super-secret" {
		t.Fatalf("upstream key = %q, want original key", upstreamKey)
	}
	if strings.Contains(rr.Body.String(), "super-secret") {
		t.Fatal("validation response leaked API key")
	}
}
