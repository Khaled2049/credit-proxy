package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	authn "github.com/kh1011/creditproxy/pkg/auth"
	"github.com/kh1011/creditproxy/pkg/contracts"
	"github.com/kh1011/creditproxy/pkg/httpx"
)

// newTestServer wires a gateway server whose usage upstream is the given stub.
// Dev auth mode makes ResolveBillingUser pass the supplied user_id through
// unchanged, so tests can focus on the proxy behaviour.
func newTestServer(usageURL string) *server {
	return &server{
		usageURL: usageURL,
		client:   httpx.NewHTTPClient(5 * time.Second),
		verifier: authn.NewVerifier(authn.Config{Mode: authn.ModeDev}),
	}
}

// TestHandleGenerateConvertsTokensToCredits verifies the full reserve→commit
// flow: the reservation holds the TRUE token ceiling (prompt + maxOutputTokens)
// converted to credits, and commit reconciles to the provider's REAL usage,
// also converted — both via TOKENS_PER_CREDIT.
func TestHandleGenerateConvertsTokensToCredits(t *testing.T) {
	var reservedCredits, committedCredits int64
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/reservations" && r.Method == http.MethodPost:
			var req contracts.ReservationRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			reservedCredits = req.EstimatedCredits
			httpx.WriteJSON(w, http.StatusOK, contracts.ReservationResponse{
				ReservationID: req.ReservationID, UserID: req.UserID,
				Reserved: req.EstimatedCredits, Status: "reserved",
			})
		case r.URL.Path == "/v1/generate" && r.Method == http.MethodPost:
			httpx.WriteJSON(w, http.StatusOK, contracts.GenerateResponse{
				Output: "hi", Model: "mock",
				// Real provider usage: 450 total tokens.
				Usage: contracts.GenerateUsage{PromptTokens: 200, CompletionTokens: 250, TotalTokens: 450},
			})
		case strings.HasSuffix(r.URL.Path, "/commit") && r.Method == http.MethodPost:
			var req contracts.CommitReservationRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			committedCredits = req.ActualCredits
			httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "committed"})
		case r.URL.Path == "/v1/events" && r.Method == http.MethodPost:
			httpx.WriteJSON(w, http.StatusOK, map[string]any{})
		default:
			http.NotFound(w, r)
		}
	}))
	defer stub.Close()

	s := &server{
		usageURL:        stub.URL,
		llmURL:          stub.URL,
		ledgerURL:       stub.URL,
		client:          httpx.NewHTTPClient(5 * time.Second),
		verifier:        authn.NewVerifier(authn.Config{Mode: authn.ModeDev}),
		maxOutputTokens: 8192,
		maxPromptChars:  64000,
		tokensPerCredit: 100,
		rateLimiter:     newUserRateLimiter(10),
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/generate",
		strings.NewReader(`{"user_id":"user123","prompt":"hello world","max_output_tokens":1024}`))
	s.handleGenerate(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	// prompt "hello world" = 11 bytes + 32 overhead; ceiling = 43 + 1024 = 1067 → ceil(1067/100) = 11
	if reservedCredits != 11 {
		t.Errorf("reserved credits = %d, want 11 (true ceiling / 100)", reservedCredits)
	}
	// real usage 450 tokens → ceil(450/100) = 5
	if committedCredits != 5 {
		t.Errorf("committed credits = %d, want 5 (real usage / 100)", committedCredits)
	}
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out["estimated_credits"].(float64) != 11 || out["actual_credits"].(float64) != 5 {
		t.Errorf("response estimated=%v actual=%v, want 11/5", out["estimated_credits"], out["actual_credits"])
	}
}

func TestClassifyReservationStatus(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"real insufficient credits", &httpx.UpstreamError{StatusCode: 402, Body: "insufficient credits"}, 402},
		{"platform daily cap", &httpx.UpstreamError{StatusCode: 429, Body: "platform daily request limit reached"}, 429},
		{"usage service internal error is not the user's fault", &httpx.UpstreamError{StatusCode: 500, Body: "redis: connection refused"}, http.StatusServiceUnavailable},
		{"malformed reservation request is a gateway bug, not user's fault", &httpx.UpstreamError{StatusCode: 400, Body: "user_id and positive estimated_credits are required"}, http.StatusServiceUnavailable},
		{"network error, no status code available", errors.New("dial tcp: connection refused"), http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyReservationStatus(tc.err); got != tc.want {
				t.Errorf("classifyReservationStatus(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

func TestClassifyPurchaseStatus(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"purchase api disabled surfaces as unavailable", &httpx.UpstreamError{StatusCode: 404, Body: "404 page not found"}, http.StatusServiceUnavailable},
		{"bad request passes through", &httpx.UpstreamError{StatusCode: 400, Body: "positive credits required"}, 400},
		{"usage internal error is not the user's fault", &httpx.UpstreamError{StatusCode: 500, Body: "redis down"}, http.StatusServiceUnavailable},
		{"network error, no status", errors.New("dial tcp: connection refused"), http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyPurchaseStatus(tc.err); got != tc.want {
				t.Errorf("classifyPurchaseStatus(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

func TestHandleUserBalance(t *testing.T) {
	var gotPath string
	usage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"user_id":           "user123",
			"available_credits": 1749,
			"effective_credits": 1749,
		})
	}))
	defer usage.Close()

	s := newTestServer(usage.URL)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/users/user123/balance", nil)
	s.handleUserBalance(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if gotPath != "/v1/users/user123/balance" {
		t.Errorf("usage upstream path = %q, want /v1/users/user123/balance", gotPath)
	}
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if out["available_credits"].(float64) != 1749 {
		t.Errorf("available_credits = %v, want 1749", out["available_credits"])
	}
}

func TestHandleUserBalanceBadPath(t *testing.T) {
	s := newTestServer("http://unused")
	for _, path := range []string{"/v1/users/user123", "/v1/users//balance", "/v1/users/user123/wrong"} {
		rr := httptest.NewRecorder()
		s.handleUserBalance(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusNotFound {
			t.Errorf("path %q: status = %d, want 404", path, rr.Code)
		}
	}
}

func TestHandlePurchase(t *testing.T) {
	var gotBody map[string]any
	usage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		httpx.WriteJSON(w, http.StatusOK, map[string]any{
			"user_id":           "user123",
			"purchased_credits": 10000,
			"available_credits": 11749,
		})
	}))
	defer usage.Close()

	s := newTestServer(usage.URL)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/credits/purchase",
		strings.NewReader(`{"user_id":"user123","credits":10000}`))
	s.handlePurchase(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if gotBody["user_id"] != "user123" || gotBody["credits"].(float64) != 10000 {
		t.Errorf("usage upstream body = %v, want user123/10000", gotBody)
	}
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	if out["available_credits"].(float64) != 11749 {
		t.Errorf("available_credits = %v, want 11749", out["available_credits"])
	}
}

func TestHandlePurchaseDisabled(t *testing.T) {
	usage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r) // usage returns 404 when ENABLE_PURCHASE_API is off
	}))
	defer usage.Close()

	s := newTestServer(usage.URL)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/credits/purchase",
		strings.NewReader(`{"user_id":"user123","credits":10000}`))
	s.handlePurchase(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 when purchase API disabled", rr.Code)
	}
}

func TestHandlePurchaseRejectsNonPositive(t *testing.T) {
	s := newTestServer("http://unused")
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/credits/purchase",
		strings.NewReader(`{"user_id":"user123","credits":0}`))
	s.handlePurchase(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for non-positive credits", rr.Code)
	}
}

func TestClassifyGenerationStatus(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"bad byok config", &httpx.UpstreamError{StatusCode: 400, Body: "invalid byok config"}, 400},
		{"provider auth failure", &httpx.UpstreamError{StatusCode: 401, Body: "generation failed (ref llm_1)"}, 401},
		{"provider rate limited", &httpx.UpstreamError{StatusCode: 429, Body: "generation failed (ref llm_2)"}, 429},
		{"model not found", &httpx.UpstreamError{StatusCode: 404, Body: "generation failed (ref llm_3)"}, 404},
		{"unclassified provider failure", &httpx.UpstreamError{StatusCode: 502, Body: "generation failed (ref llm_4)"}, http.StatusBadGateway},
		{"network error, no status code available", errors.New("dial tcp: connection refused"), http.StatusBadGateway},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyGenerationStatus(tc.err); got != tc.want {
				t.Errorf("classifyGenerationStatus(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

func TestHandleGenerateReservesEveryByteOfUnspacedPrompt(t *testing.T) {
	var reservedCredits int64
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/reservations":
			var req contracts.ReservationRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			reservedCredits = req.EstimatedCredits
			httpx.WriteJSON(w, http.StatusOK, contracts.ReservationResponse{ReservationID: req.ReservationID, UserID: req.UserID, Reserved: req.EstimatedCredits, Status: "reserved"})
		case r.URL.Path == "/v1/generate":
			httpx.WriteJSON(w, http.StatusOK, contracts.GenerateResponse{Output: "hi", Model: "mock", Usage: contracts.GenerateUsage{TotalTokens: 15000}})
		default:
			httpx.WriteJSON(w, http.StatusOK, map[string]any{})
		}
	}))
	defer stub.Close()

	s := &server{
		usageURL: stub.URL, llmURL: stub.URL, ledgerURL: stub.URL,
		client:          httpx.NewHTTPClient(5 * time.Second),
		verifier:        authn.NewVerifier(authn.Config{Mode: authn.ModeDev}),
		maxOutputTokens: 8192,
		maxPromptChars:  64000,
		tokensPerCredit: 100,
		rateLimiter:     newUserRateLimiter(10),
	}

	prompt := strings.Repeat("x", 60000)
	body, _ := json.Marshal(contracts.GenerateRequest{UserID: "user123", Prompt: prompt, MaxOutputTokens: 256})
	rr := httptest.NewRecorder()
	s.handleGenerate(rr, httptest.NewRequest(http.MethodPost, "/v1/generate", strings.NewReader(string(body))))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rr.Code, rr.Body.String())
	}
	if want := int64((60000 + 32 + 256 + 99) / 100); reservedCredits != want {
		t.Fatalf("reserved credits = %d, want %d", reservedCredits, want)
	}
}
