package main

import (
	"log"
	"net/http"
	"strings"

	"github.com/kh1011/creditproxy/pkg/contracts"
	"github.com/kh1011/creditproxy/pkg/httpx"
	"github.com/kh1011/creditproxy/pkg/ids"
)

func (s *server) llmHeaders() map[string]string {
	if s.internalToken == "" {
		return nil
	}
	return map[string]string{"X-Internal-Token": s.internalToken}
}

// handleProviders exposes only the adapter's curated, credential-free catalog.
// Cloud Run IAM protects the gateway in production; the catalog itself is not
// user-specific and contains no provider credentials.
func (s *server) handleProviders(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var out map[string]any
	if err := httpx.GetJSON(r.Context(), s.client, s.llmURL+"/v1/providers", &out, s.llmHeaders()); err != nil {
		log.Printf("provider catalog failed: %v", err)
		http.Error(w, "provider catalog unavailable", http.StatusBadGateway)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

func (s *server) handleProviderValidation(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req contracts.ProviderValidationRequest
	if err := httpx.ReadJSON(r, &req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Provider) == "" || strings.TrimSpace(req.APIKey) == "" {
		http.Error(w, "provider and api_key are required", http.StatusBadRequest)
		return
	}
	userID, authErr := s.verifier.ResolveBillingUser(r.Context(), r, req.UserID)
	if authErr != nil {
		http.Error(w, authErr.Message, authErr.StatusCode)
		return
	}
	if !s.rateLimiter.Allow(userID + ":provider-validation") {
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}

	upstream := map[string]any{
		"provider": req.Provider,
		"api_key":  req.APIKey,
		"model":    req.Model,
	}
	var out contracts.ProviderValidationResponse
	if err := httpx.PostJSON(r.Context(), s.client, s.llmURL+"/v1/providers/validate", upstream, &out, s.llmHeaders()); err != nil {
		log.Printf("provider validation failed user=%s provider=%s: %v", userID, req.Provider, err)
		http.Error(w, "provider validation unavailable", classifyGenerationStatus(err))
		return
	}

	_ = s.emitLedgerEvent(r.Context(), contracts.LedgerEventRequest{
		IdempotencyKey: ids.New("provider-validation"),
		UserID:         userID,
		EventType:      "byok_validation",
		Payload: map[string]any{
			"provider": req.Provider,
			"model":    req.Model,
			"valid":    out.Valid,
		},
	})
	httpx.WriteJSON(w, http.StatusOK, out)
}
