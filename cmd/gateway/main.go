package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/kh1011/creditproxy/pkg/contracts"
	"github.com/kh1011/creditproxy/pkg/httpx"
	"github.com/kh1011/creditproxy/pkg/ids"
	"github.com/kh1011/creditproxy/pkg/tokens"
	"github.com/kh1011/creditproxy/pkg/version"
)

type server struct {
	usageURL  string
	llmURL    string
	ledgerURL string
	client    *http.Client
}

func main() {
	addr := getenv("GATEWAY_ADDR", ":8080")
	s := &server{
		usageURL:  strings.TrimRight(getenv("USAGE_SERVICE_URL", "http://usage:8081"), "/"),
		llmURL:    strings.TrimRight(getenv("LLM_PROXY_URL", "http://llmproxy:8082"), "/"),
		ledgerURL: strings.TrimRight(getenv("LEDGER_SERVICE_URL", "http://ledger:8083"), "/"),
		client:    httpx.NewHTTPClient(30 * time.Second),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/v1/generate", s.handleGenerate)

	log.Printf("gateway listening on %s", addr)
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
	if req.UserID == "" || req.Prompt == "" {
		http.Error(w, "user_id and prompt are required", http.StatusBadRequest)
		return
	}
	if req.MaxOutputTokens <= 0 {
		req.MaxOutputTokens = 256
	}
	if req.IdempotencyKey == "" {
		req.IdempotencyKey = strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	}
	if req.IdempotencyKey == "" {
		req.IdempotencyKey = ids.New("idem")
	}

	promptToks, estimatedTotal := tokens.EstimatePromptAndMaxCompletion(req.Prompt, req.MaxOutputTokens)
	reservationID := ids.New("res")

	reserveReq := contracts.ReservationRequest{
		ReservationID:    reservationID,
		UserID:           req.UserID,
		EstimatedCredits: estimatedTotal,
		TTLSeconds:       180,
	}
	var reserveResp contracts.ReservationResponse
	if err := httpx.PostJSON(r.Context(), s.client, s.usageURL+"/v1/reservations", reserveReq, &reserveResp, nil); err != nil {
		http.Error(w, "reserve credits: "+err.Error(), http.StatusPaymentRequired)
		return
	}

	_ = s.emitLedgerEvent(r.Context(), contracts.LedgerEventRequest{
		IdempotencyKey: req.IdempotencyKey + ":reserve",
		UserID:         req.UserID,
		ReservationID:  reservationID,
		EventType:      "credits_reserved",
		Credits:        estimatedTotal,
		Payload:        map[string]any{"prompt_tokens": promptToks, "estimated_total_tokens": estimatedTotal},
	})

	genReq := contracts.GenerateRequest{
		UserID:          req.UserID,
		Prompt:          req.Prompt,
		MaxOutputTokens: req.MaxOutputTokens,
		Temperature:     req.Temperature,
		ForceMock:       req.ForceMock,
		ReservationID:   reservationID,
		IdempotencyKey:  req.IdempotencyKey,
	}
	var genResp contracts.GenerateResponse
	if err := httpx.PostJSON(r.Context(), s.client, s.llmURL+"/v1/generate", genReq, &genResp, nil); err != nil {
		_ = s.releaseReservation(r.Context(), req.UserID, reservationID, "llm_failure")
		_ = s.emitLedgerEvent(r.Context(), contracts.LedgerEventRequest{
			IdempotencyKey: req.IdempotencyKey + ":release",
			UserID:         req.UserID,
			ReservationID:  reservationID,
			EventType:      "credits_released",
			Credits:        estimatedTotal,
			Payload:        map[string]any{"reason": "llm_failure"},
		})
		http.Error(w, "llm proxy failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	actualCredits := genResp.Usage.TotalTokens
	if err := s.commitReservation(r.Context(), req.UserID, reservationID, actualCredits); err != nil {
		_ = s.emitLedgerEvent(r.Context(), contracts.LedgerEventRequest{
			IdempotencyKey: req.IdempotencyKey + ":commit_failed",
			UserID:         req.UserID,
			ReservationID:  reservationID,
			EventType:      "commit_failed",
			Credits:        actualCredits,
			Payload:        map[string]any{"error": err.Error()},
		})
		http.Error(w, "commit reservation failed: "+err.Error(), http.StatusConflict)
		return
	}

	_ = s.emitLedgerEvent(r.Context(), contracts.LedgerEventRequest{
		IdempotencyKey: req.IdempotencyKey + ":commit",
		UserID:         req.UserID,
		ReservationID:  reservationID,
		EventType:      "credits_committed",
		Credits:        actualCredits,
		Payload: map[string]any{
			"estimated_credits": estimatedTotal,
			"actual_tokens":     genResp.Usage,
			"model":             genResp.Model,
		},
	})

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"reservation_id":    reservationID,
		"idempotency_key":   req.IdempotencyKey,
		"estimated_credits": estimatedTotal,
		"actual_credits":    actualCredits,
		"response":          genResp,
	})
}

func (s *server) commitReservation(ctx context.Context, userID, reservationID string, actualCredits int64) error {
	url := fmt.Sprintf("%s/v1/reservations/%s/commit?user_id=%s", s.usageURL, reservationID, userID)
	return httpx.PostJSON(ctx, s.client, url, contracts.CommitReservationRequest{ActualCredits: actualCredits}, &map[string]any{}, nil)
}

func (s *server) releaseReservation(ctx context.Context, userID, reservationID, reason string) error {
	url := fmt.Sprintf("%s/v1/reservations/%s/release?user_id=%s", s.usageURL, reservationID, userID)
	return httpx.PostJSON(ctx, s.client, url, contracts.ReleaseReservationRequest{Reason: reason}, &map[string]any{}, nil)
}

func (s *server) emitLedgerEvent(ctx context.Context, event contracts.LedgerEventRequest) error {
	return httpx.PostJSON(ctx, s.client, s.ledgerURL+"/v1/events", event, &map[string]any{}, nil)
}

func getenv(key, fallback string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	return v
}

func getint(key string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	i, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return i
}
