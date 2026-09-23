package main

import (
	"context"
	"errors"
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
	maxChatInput    int
	tokensPerCredit int64
	rateLimiter     *userRateLimiter
	internalToken   string

	streamClient             *http.Client
	chatRateLimiter          *userRateLimiter
	platformInferenceEnabled bool
	localUnmetered           bool
}

func main() {
	addr := getenv("GATEWAY_ADDR", ":8080")
	authMode, err := authn.ParseMode(os.Getenv("AUTH_MODE"))
	if err != nil {
		log.Fatal(err)
	}
	authConfig := authn.Config{
		Mode:                    authMode,
		GCPAudience:             getenv("GCP_AUDIENCE", ""),
		AllowedCallerIdentities: splitCSV(getenv("GCP_ALLOWED_CALLER_SA", "")),
		FirebaseProjectID:       getenv("FIREBASE_PROJECT_ID", ""),
	}
	if err := authConfig.Validate(); err != nil {
		log.Fatal(err)
	}
	if authMode == authn.ModeDev {
		log.Printf("WARNING: AUTH_MODE=dev accepts any caller and trusts the user_id in the request body; never expose this gateway beyond localhost")
	}
	s := &server{
		usageURL:        strings.TrimRight(getenv("USAGE_SERVICE_URL", "http://usage:8081"), "/"),
		llmURL:          strings.TrimRight(getenv("LLM_PROXY_URL", "http://llmproxy:8082"), "/"),
		ledgerURL:       strings.TrimRight(getenv("LEDGER_SERVICE_URL", "http://ledger:8083"), "/"),
		client:          httpx.NewHTTPClient(30 * time.Second),
		verifier:        authn.NewVerifier(authConfig),
		maxOutputTokens: getenvInt64("MAX_OUTPUT_TOKENS", 8192),
		maxPromptChars:  int(getenvInt64("MAX_PROMPT_CHARS", 64000)),
		maxChatInput:    int(getenvInt64("MAX_CHAT_INPUT_BYTES", 262144)),
		tokensPerCredit: getenvInt64("TOKENS_PER_CREDIT", 100),
		rateLimiter:     newUserRateLimiter(int(getenvInt64("MAX_REQUESTS_PER_MINUTE_PER_USER", 10))),
		internalToken:   strings.TrimSpace(os.Getenv("INTERNAL_SERVICE_TOKEN")),

		streamClient: httpx.NewStreamingHTTPClient(30 * time.Second),
		// One assistant run is several /v1/chat calls, so chat gets its own
		// bucket; sharing /v1/generate's limit of 10/min would trip on a
		// writer's second question.
		chatRateLimiter:          newUserRateLimiter(int(getenvInt64("MAX_CHAT_REQUESTS_PER_MINUTE_PER_USER", 60))),
		platformInferenceEnabled: getenvBool("PLATFORM_INFERENCE_ENABLED", true),
		localUnmetered:           getenvBool("LOCAL_UNMETERED", false),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/v1/generate", s.handleGenerate)
	mux.HandleFunc("/v1/chat", s.handleChat)
	mux.HandleFunc("/v1/providers", s.handleProviders)
	mux.HandleFunc("/v1/providers/validate", s.handleProviderValidation)
	mux.HandleFunc("/v1/users/", s.handleUserBalance)
	mux.HandleFunc("/v1/credits/purchase", s.handlePurchase)

	log.Printf("gateway listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func classifyReservationStatus(err error) int {
	var upErr *httpx.UpstreamError
	if errors.As(err, &upErr) {
		switch upErr.StatusCode {
		case http.StatusPaymentRequired, http.StatusTooManyRequests:
			return upErr.StatusCode
		}
	}
	return http.StatusServiceUnavailable
}

// classifyPurchaseStatus maps upstream usage-service failures for the purchase
// proxy. A 404 means the usage purchase API is disabled (ENABLE_PURCHASE_API);
// surface it as 503 so it isn't confused with an unknown gateway route.
func classifyPurchaseStatus(err error) int {
	var upErr *httpx.UpstreamError
	if errors.As(err, &upErr) {
		switch upErr.StatusCode {
		case http.StatusNotFound:
			return http.StatusServiceUnavailable
		case http.StatusBadRequest, http.StatusPaymentRequired, http.StatusTooManyRequests:
			return upErr.StatusCode
		}
	}
	return http.StatusServiceUnavailable
}

func classifyGenerationStatus(err error) int {
	var upErr *httpx.UpstreamError
	if errors.As(err, &upErr) {
		switch upErr.StatusCode {
		case http.StatusBadRequest, http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusNotFound, http.StatusNotImplemented:
			return upErr.StatusCode
		}
	}
	return http.StatusBadGateway
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

	// Correlation id for tying a client-facing generic error back to the
	// detailed server log (we never echo upstream error bodies to the caller).
	reqID := ids.New("req")

	isBYOK := req.BYOKProvider != "" && req.BYOKApiKey != ""

	if !isBYOK && !req.ForceMock && !s.platformInferenceEnabled {
		http.Error(w, "platform inference is disabled (ref "+reqID+")", http.StatusServiceUnavailable)
		return
	}

	promptToks, estimatedTokens := tokens.Ceiling(len(req.Prompt), req.MaxOutputTokens)
	estimatedCredits := tokens.ToCredits(estimatedTokens, s.tokensPerCredit)
	reservationID := ""

	if !isBYOK {
		reservationID = ids.New("res")
		reserveReq := contracts.ReservationRequest{
			ReservationID:    reservationID,
			UserID:           billingUserID,
			EstimatedCredits: estimatedCredits,
			TTLSeconds:       180,
		}
		var reserveResp contracts.ReservationResponse
		if err := httpx.PostJSON(r.Context(), s.client, s.usageURL+"/v1/reservations", reserveReq, &reserveResp, nil); err != nil {
			log.Printf("reserve credits failed req=%s user=%s: %v", reqID, billingUserID, err)
			http.Error(w, "unable to reserve credits (ref "+reqID+")", classifyReservationStatus(err))
			return
		}
		_ = s.emitLedgerEvent(r.Context(), contracts.LedgerEventRequest{
			IdempotencyKey: req.IdempotencyKey + ":reserve",
			UserID:         billingUserID,
			ReservationID:  reservationID,
			EventType:      "credits_reserved",
			Credits:        estimatedCredits,
			Payload:        map[string]any{"prompt_tokens": promptToks, "estimated_total_tokens": estimatedTokens},
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
				Credits:        estimatedCredits,
				Payload:        map[string]any{"reason": "llm_failure"},
			})
		}
		log.Printf("llm proxy failed req=%s user=%s byok=%t: %v", reqID, billingUserID, isBYOK, err)
		http.Error(w, "upstream generation failed (ref "+reqID+")", classifyGenerationStatus(err))
		return
	}

	// Reconcile to the provider's real usage (llmproxy reports actual tokens when
	// the provider returns them; otherwise a heuristic estimate).
	actualTokens := genResp.Usage.TotalTokens
	actualCredits := tokens.ToCredits(actualTokens, s.tokensPerCredit)
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
			log.Printf("commit reservation failed req=%s user=%s res=%s: %v", reqID, billingUserID, reservationID, err)
			http.Error(w, "commit reservation failed (ref "+reqID+")", http.StatusConflict)
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
			"estimated_credits": estimatedCredits,
			"actual_tokens":     genResp.Usage,
			"model":             genResp.Model,
			"byok":              isBYOK,
		},
	})

	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"reservation_id":    reservationID,
		"idempotency_key":   req.IdempotencyKey,
		"estimated_credits": estimatedCredits,
		"actual_credits":    actualCredits,
		"byok":              isBYOK,
		"response":          genResp,
	})
}

// handleUserBalance proxies GET /v1/users/{userId}/balance to the usage service.
// The path-supplied user id is authenticated/cross-checked against the caller's
// forwarded identity via the same verifier used by generate.
func (s *server) handleUserBalance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1/users/")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[1] != "balance" || parts[0] == "" {
		http.NotFound(w, r)
		return
	}
	billingUserID, authErr := s.verifier.ResolveBillingUser(r.Context(), r, parts[0])
	if authErr != nil {
		http.Error(w, authErr.Message, authErr.StatusCode)
		return
	}

	reqID := ids.New("req")
	var balance contracts.BalanceResponse
	url := fmt.Sprintf("%s/v1/users/%s/balance", s.usageURL, billingUserID)
	if err := httpx.GetJSON(r.Context(), s.client, url, &balance, nil); err != nil {
		log.Printf("balance lookup failed req=%s user=%s: %v", reqID, billingUserID, err)
		http.Error(w, "unable to fetch balance (ref "+reqID+")", classifyReservationStatus(err))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, balance)
}

// handlePurchase proxies POST /v1/credits/purchase to the usage service. The
// usage endpoint is itself gated behind ENABLE_PURCHASE_API; when disabled it
// returns 404, which we surface as 503 so callers can distinguish it from an
// unknown route.
func (s *server) handlePurchase(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req contracts.PurchaseCreditsRequest
	if err := httpx.ReadJSON(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Credits <= 0 {
		http.Error(w, "credits must be positive", http.StatusBadRequest)
		return
	}
	billingUserID, authErr := s.verifier.ResolveBillingUser(r.Context(), r, req.UserID)
	if authErr != nil {
		http.Error(w, authErr.Message, authErr.StatusCode)
		return
	}

	reqID := ids.New("req")
	purchaseReq := contracts.PurchaseCreditsRequest{UserID: billingUserID, Credits: req.Credits}
	var out map[string]any
	if err := httpx.PostJSON(r.Context(), s.client, s.usageURL+"/v1/credits/purchase", purchaseReq, &out, nil); err != nil {
		log.Printf("purchase failed req=%s user=%s: %v", reqID, billingUserID, err)
		http.Error(w, "unable to purchase credits (ref "+reqID+")", classifyPurchaseStatus(err))
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
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

func getenvBool(key string, fallback bool) bool {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	return strings.EqualFold(raw, "true")
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
