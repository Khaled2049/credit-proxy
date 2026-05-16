package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	authn "github.com/kh1011/creditproxy/pkg/auth"
	"github.com/kh1011/creditproxy/pkg/contracts"
	"github.com/kh1011/creditproxy/pkg/httpx"
	"github.com/kh1011/creditproxy/pkg/ids"
	"github.com/kh1011/creditproxy/pkg/tokens"
	"github.com/kh1011/creditproxy/pkg/version"
	"golang.org/x/time/rate"
)

type server struct {
	usageURL        string
	llmURL          string
	ledgerURL       string
	client          *http.Client
	verifier        *authn.Verifier
	maxOutputTokens int64
	maxPromptChars  int
	rateLimiter     *userRateLimiter
	internalToken   string
}

func main() {
	addr := getenv("GATEWAY_ADDR", ":8080")
	s := &server{
		usageURL:  strings.TrimRight(getenv("USAGE_SERVICE_URL", "http://usage:8081"), "/"),
		llmURL:    strings.TrimRight(getenv("LLM_PROXY_URL", "http://llmproxy:8082"), "/"),
		ledgerURL: strings.TrimRight(getenv("LEDGER_SERVICE_URL", "http://ledger:8083"), "/"),
		client:    httpx.NewHTTPClient(30 * time.Second),
		verifier: authn.NewVerifier(authn.Config{
			Mode:                    authn.ParseMode(getenv("AUTH_MODE", "dev")),
			GCPAudience:             getenv("GCP_AUDIENCE", ""),
			AllowedCallerIdentities: splitCSV(getenv("GCP_ALLOWED_CALLER_SA", "")),
			FirebaseProjectID:       getenv("FIREBASE_PROJECT_ID", ""),
		}),
		maxOutputTokens: getenvInt64("MAX_OUTPUT_TOKENS", 8192),
		maxPromptChars:  int(getenvInt64("MAX_PROMPT_CHARS", 64000)),
		rateLimiter:     newUserRateLimiter(int(getenvInt64("MAX_REQUESTS_PER_MINUTE_PER_USER", 10))),
		internalToken:   strings.TrimSpace(os.Getenv("INTERNAL_SERVICE_TOKEN")),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
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
	if req.Prompt == "" {
		http.Error(w, "prompt is required", http.StatusBadRequest)
		return
	}
	billingUserID, authErr := s.verifier.ResolveBillingUser(r.Context(), r, req.UserID)
	if authErr != nil {
		http.Error(w, authErr.Message, authErr.StatusCode)
		return
	}
	if req.MaxOutputTokens <= 0 {
		req.MaxOutputTokens = 256
	}
	if req.MaxOutputTokens > s.maxOutputTokens {
		http.Error(w, fmt.Sprintf("max_output_tokens must be <= %d", s.maxOutputTokens), http.StatusBadRequest)
		return
	}
	if len(req.Prompt) > s.maxPromptChars {
		http.Error(w, fmt.Sprintf("prompt must be <= %d characters", s.maxPromptChars), http.StatusBadRequest)
		return
	}
	if !s.rateLimiter.Allow(billingUserID) {
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}
	if req.IdempotencyKey == "" {
		req.IdempotencyKey = strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	}
	if req.IdempotencyKey == "" {
		req.IdempotencyKey = ids.New("idem")
	}

	isBYOK := req.BYOKProvider != "" && req.BYOKApiKey != ""

	promptToks, estimatedTotal := tokens.EstimatePromptAndMaxCompletion(req.Prompt, req.MaxOutputTokens)
	reservationID := ""

	if !isBYOK {
		reservationID = ids.New("res")
		reserveReq := contracts.ReservationRequest{
			ReservationID:    reservationID,
			UserID:           billingUserID,
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
			UserID:         billingUserID,
			ReservationID:  reservationID,
			EventType:      "credits_reserved",
			Credits:        estimatedTotal,
			Payload:        map[string]any{"prompt_tokens": promptToks, "estimated_total_tokens": estimatedTotal},
		})
	}

	genReq := contracts.GenerateRequest{
		UserID:          billingUserID,
		Prompt:          req.Prompt,
		MaxOutputTokens: req.MaxOutputTokens,
		Temperature:     req.Temperature,
		ForceMock:       req.ForceMock,
		ReservationID:   reservationID,
		IdempotencyKey:  req.IdempotencyKey,
		BYOKProvider:    req.BYOKProvider,
		BYOKApiKey:      req.BYOKApiKey,
		BYOKModel:       req.BYOKModel,
	}
	var llmHeaders map[string]string
	if s.internalToken != "" {
		llmHeaders = map[string]string{"X-Internal-Token": s.internalToken}
	}
	var genResp contracts.GenerateResponse
	if err := httpx.PostJSON(r.Context(), s.client, s.llmURL+"/v1/generate", genReq, &genResp, llmHeaders); err != nil {
		if !isBYOK {
			_ = s.releaseReservation(r.Context(), billingUserID, reservationID, "llm_failure")
			_ = s.emitLedgerEvent(r.Context(), contracts.LedgerEventRequest{
				IdempotencyKey: req.IdempotencyKey + ":release",
				UserID:         billingUserID,
				ReservationID:  reservationID,
				EventType:      "credits_released",
				Credits:        estimatedTotal,
				Payload:        map[string]any{"reason": "llm_failure"},
			})
		}
		http.Error(w, "llm proxy failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	actualCredits := genResp.Usage.TotalTokens
	if !isBYOK {
		if err := s.commitReservation(r.Context(), billingUserID, reservationID, actualCredits); err != nil {
			_ = s.emitLedgerEvent(r.Context(), contracts.LedgerEventRequest{
				IdempotencyKey: req.IdempotencyKey + ":commit_failed",
				UserID:         billingUserID,
				ReservationID:  reservationID,
				EventType:      "commit_failed",
				Credits:        actualCredits,
				Payload:        map[string]any{"error": err.Error()},
			})
			http.Error(w, "commit reservation failed: "+err.Error(), http.StatusConflict)
			return
		}
	}

	eventType := "credits_committed"
	if isBYOK {
		eventType = "byok_generate"
	}
	_ = s.emitLedgerEvent(r.Context(), contracts.LedgerEventRequest{
		IdempotencyKey: req.IdempotencyKey + ":commit",
		UserID:         billingUserID,
		ReservationID:  reservationID,
		EventType:      eventType,
		Credits:        actualCredits,
		Payload: map[string]any{
			"estimated_credits": estimatedTotal,
			"actual_tokens":     genResp.Usage,
			"model":             genResp.Model,
			"byok":              isBYOK,
		},
	})

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"reservation_id":    reservationID,
		"idempotency_key":   req.IdempotencyKey,
		"estimated_credits": estimatedTotal,
		"actual_credits":    actualCredits,
		"byok":              isBYOK,
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

func splitCSV(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func getenvInt64(key string, fallback int64) int64 {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

type userRateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*limiterEntry
	limit   rate.Limit
	burst   int
}

type limiterEntry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

func newUserRateLimiter(requestsPerMinute int) *userRateLimiter {
	if requestsPerMinute <= 0 {
		requestsPerMinute = 10
	}
	limit := rate.Every(time.Minute / time.Duration(requestsPerMinute))
	return &userRateLimiter{
		buckets: make(map[string]*limiterEntry),
		limit:   limit,
		burst:   requestsPerMinute,
	}
}

func (r *userRateLimiter) Allow(userID string) bool {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return false
	}
	now := time.Now()

	r.mu.Lock()
	defer r.mu.Unlock()

	entry, ok := r.buckets[userID]
	if !ok {
		entry = &limiterEntry{
			limiter:  rate.NewLimiter(r.limit, r.burst),
			lastSeen: now,
		}
		r.buckets[userID] = entry
	}
	entry.lastSeen = now

	for id, candidate := range r.buckets {
		if now.Sub(candidate.lastSeen) > 10*time.Minute {
			delete(r.buckets, id)
		}
	}

	return entry.limiter.Allow()
}
