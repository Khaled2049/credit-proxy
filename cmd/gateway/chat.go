package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/kh1011/creditproxy/pkg/contracts"
	"github.com/kh1011/creditproxy/pkg/httpx"
	"github.com/kh1011/creditproxy/pkg/ids"
	"github.com/kh1011/creditproxy/pkg/tokens"
)

// settleTimeout bounds the credit settlement calls made after a stream ends.
// They run on a context detached from the request, because the most common
// reason a stream ends early is that the client disconnected — which cancels
// the request context, which would otherwise cancel the commit too.
const settleTimeout = 10 * time.Second

// Who pays for a run. Only billingPlatform touches credits; BYOK, validated
// local inference, and the explicit test mock are audited but unmetered.
const (
	billingPlatform = "platform"
	billingBYOK     = "byok"
	billingLocal    = "local"
	billingMock     = "mock"
)

func (s *server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeChatError(w, http.StatusMethodNotAllowed, "invalid_request", "")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeChatError(w, http.StatusInternalServerError, "internal_error", "")
		return
	}

	var req contracts.ChatRequest
	if err := httpx.ReadJSON(r, &req); err != nil {
		writeChatError(w, http.StatusBadRequest, "invalid_request", "")
		return
	}
	if req.Version != contracts.ChatContractVersion {
		writeChatError(w, http.StatusBadRequest, "unsupported_contract_version", "")
		return
	}
	if len(req.Messages) == 0 {
		writeChatError(w, http.StatusBadRequest, "invalid_request", "")
		return
	}
	billingUserID, authErr := s.verifier.ResolveBillingUser(r.Context(), r, req.UserID)
	if authErr != nil {
		writeChatError(w, authErr.StatusCode, "unauthorized", "")
		return
	}
	if req.MaxOutputTokens <= 0 || req.MaxOutputTokens > s.maxOutputTokens {
		writeChatError(w, http.StatusBadRequest, "invalid_max_output_tokens", "")
		return
	}
	input, err := chatInput(req)
	if err != nil {
		writeChatError(w, http.StatusBadRequest, "invalid_request", "")
		return
	}
	if len(input) > s.maxChatInput {
		writeChatError(w, http.StatusBadRequest, "prompt_too_large", "")
		return
	}
	if !s.chatRateLimiter.Allow(billingUserID) {
		writeChatError(w, http.StatusTooManyRequests, "rate_limited", "")
		return
	}
	if req.IdempotencyKey == "" {
		req.IdempotencyKey = strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	}
	if req.IdempotencyKey == "" {
		req.IdempotencyKey = ids.New("idem")
	}

	reqID := ids.New("req")
	isBYOK := req.BYOKProvider != "" && req.BYOKApiKey != ""

	// Four billing modes, and only one of them touches credits. "local" is a
	// validated local model (LOCAL_UNMETERED); llmproxy refuses to start if that
	// flag is set without a genuinely local provider, so it cannot be flipped
	// into free hosted inference.
	billing := billingPlatform
	switch {
	case isBYOK:
		billing = billingBYOK
	case req.ForceMock:
		billing = billingMock
	case s.localUnmetered:
		billing = billingLocal
	}
	metered := billing == billingPlatform

	// The kill switch refuses platform-funded inference only. BYOK, local and an
	// explicit mock still work, so flipping it degrades the platform instead of
	// taking every path down with it.
	if metered && !s.platformInferenceEnabled {
		writeChatError(w, http.StatusServiceUnavailable, "platform_inference_disabled", reqID)
		return
	}

	promptTokens, estimatedTokens := tokens.Ceiling(len(input), req.MaxOutputTokens)
	estimatedCredits := tokens.ToCredits(estimatedTokens, s.tokensPerCredit)
	reservationID := ""

	// Reserve before a single byte is written, so a refusal is still an HTTP
	// status the caller can branch on rather than an error frame arriving after
	// a 200.
	if metered {
		reservationID = ids.New("res")
		reserveReq := contracts.ReservationRequest{
			ReservationID:    reservationID,
			UserID:           billingUserID,
			EstimatedCredits: estimatedCredits,
			TTLSeconds:       600,
		}
		var reserveResp contracts.ReservationResponse
		if err := httpx.PostJSON(r.Context(), s.client, s.usageURL+"/v1/reservations", reserveReq, &reserveResp, nil); err != nil {
			log.Printf("chat reserve failed req=%s user=%s: %v", reqID, billingUserID, err)
			writeChatError(w, classifyReservationStatus(err), reservationErrorCode(err), reqID)
			return
		}
		_ = s.emitLedgerEvent(r.Context(), contracts.LedgerEventRequest{
			IdempotencyKey: req.IdempotencyKey + ":reserve",
			UserID:         billingUserID,
			ReservationID:  reservationID,
			EventType:      "credits_reserved",
			Credits:        estimatedCredits,
			Payload:        map[string]any{"prompt_tokens": promptTokens, "estimated_total_tokens": estimatedTokens, "surface": "chat"},
		})
	}

	upstream := req
	upstream.UserID = billingUserID
	upstream.ReservationID = reservationID
	upstream.IdempotencyKey = req.IdempotencyKey

	var llmHeaders map[string]string
	if s.internalToken != "" {
		llmHeaders = map[string]string{"X-Internal-Token": s.internalToken}
	}

	if !req.Stream {
		s.bufferedChat(w, r, upstream, llmHeaders, settleParams{
			reqID:            reqID,
			userID:           billingUserID,
			reservationID:    reservationID,
			idempotencyKey:   req.IdempotencyKey,
			billing:          billing,
			estimatedCredits: estimatedCredits,
		})
		return
	}

	var (
		opened          bool
		billableStarted bool
		sawUsage        bool
		sawError        bool
		actualCredits   int64
	)

	relayErr := httpx.PostSSE(r.Context(), s.streamClient, s.llmURL+"/v1/chat", upstream, llmHeaders, func(raw []byte) error {
		var event contracts.ChatEvent
		if err := json.Unmarshal(raw, &event); err != nil {
			return fmt.Errorf("decode upstream event: %w", err)
		}
		switch event.Type {
		case contracts.ChatEventTextDelta, contracts.ChatEventToolCallDelta:
			billableStarted = true
		case contracts.ChatEventUsage:
			sawUsage = true
			// llmproxy reports tokens; credits are the gateway's to decide.
			if !metered {
				event.Credits = 0
			} else if event.Usage != nil {
				actualCredits = tokens.ToCredits(event.Usage.TotalTokens, s.tokensPerCredit)
				event.Credits = actualCredits
			}
		case contracts.ChatEventError:
			sawError = true
		}

		payload, err := json.Marshal(event)
		if err != nil {
			return fmt.Errorf("encode event: %w", err)
		}
		if !opened {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.Header().Set("X-Accel-Buffering", "no")
			w.WriteHeader(http.StatusOK)
			opened = true
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	})

	failed := relayErr != nil || sawError
	if relayErr != nil {
		log.Printf("chat relay failed req=%s user=%s billing=%s opened=%t billable=%t: %v",
			reqID, billingUserID, billing, opened, billableStarted, relayErr)
	}

	s.settleChat(r, settleParams{
		reqID:            reqID,
		userID:           billingUserID,
		reservationID:    reservationID,
		idempotencyKey:   req.IdempotencyKey,
		billing:          billing,
		failed:           failed,
		billableStarted:  billableStarted,
		sawUsage:         sawUsage,
		actualCredits:    actualCredits,
		estimatedCredits: estimatedCredits,
	})

	if relayErr == nil {
		return
	}
	if !opened {
		writeChatError(w, classifyGenerationStatus(relayErr), relayErrorCode(relayErr), reqID)
		return
	}
	// The client is already reading a 200. If llmproxy did not send a terminal
	// error frame of its own, send one, so a dropped upstream is never
	// indistinguishable from a finished run.
	if !sawError && r.Context().Err() == nil {
		payload, err := json.Marshal(contracts.ChatEvent{
			Type:         contracts.ChatEventError,
			FinishReason: contracts.FinishError,
			Error:        &contracts.ChatError{Code: relayErrorCode(relayErr), Message: "chat failed (ref " + reqID + ")", Retryable: true},
		})
		if err == nil {
			fmt.Fprintf(w, "data: %s\n\n", payload)
			flusher.Flush()
		}
	}
}

// bufferedChat serves stream: false by letting llmproxy aggregate the same
// provider stream into one ChatResponse. Nothing is delivered incrementally, so
// the outcome is the two-way decision /v1/generate already makes: commit the
// reported usage, or release.
func (s *server) bufferedChat(w http.ResponseWriter, r *http.Request, upstream contracts.ChatRequest, headers map[string]string, p settleParams) {
	var resp contracts.ChatResponse
	if err := httpx.PostJSON(r.Context(), s.streamClient, s.llmURL+"/v1/chat", upstream, &resp, headers); err != nil {
		log.Printf("chat relay failed req=%s user=%s billing=%s buffered: %v", p.reqID, p.userID, p.billing, err)
		p.failed = true
		s.settleChat(r, p)
		writeChatError(w, classifyGenerationStatus(err), relayErrorCode(err), p.reqID)
		return
	}

	if p.billing == billingPlatform {
		p.actualCredits = tokens.ToCredits(resp.Usage.TotalTokens, s.tokensPerCredit)
		resp.Credits = p.actualCredits
	} else {
		resp.Credits = 0
	}
	p.sawUsage = resp.Usage.TotalTokens > 0
	s.settleChat(r, p)
	httpx.WriteJSON(w, http.StatusOK, resp)
}

type settleParams struct {
	reqID            string
	userID           string
	reservationID    string
	idempotencyKey   string
	billing          string
	failed           bool
	billableStarted  bool
	sawUsage         bool
	actualCredits    int64
	estimatedCredits int64
}

// settleChat applies the billing outcome of a finished stream:
//
//	usage reported                 -> commit the reported credits
//	failed before any output       -> release
//	anything else                  -> commit the full hold
//
// The last case is the conservative unknown-usage policy: a stream that died
// mid-flight, a client that hung up, or a provider that never sent a usage
// block all leave the platform unable to know what it was charged, so it
// assumes the ceiling it already reserved.
func (s *server) settleChat(r *http.Request, p settleParams) {
	if p.billing != billingPlatform {
		eventType := "byok_chat"
		switch p.billing {
		case billingLocal:
			eventType = "local_generate"
		case billingMock:
			eventType = "mock_chat"
		}
		_ = s.emitLedgerEvent(r.Context(), contracts.LedgerEventRequest{
			IdempotencyKey: p.idempotencyKey + ":commit",
			UserID:         p.userID,
			EventType:      eventType,
			Payload:        map[string]any{"failed": p.failed, "surface": "chat"},
		})
		return
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), settleTimeout)
	defer cancel()

	switch {
	case !p.failed && p.sawUsage:
		s.commitChat(ctx, p, p.actualCredits, "credits_committed", "reported")
	case p.failed && !p.billableStarted:
		if err := s.releaseReservation(ctx, p.userID, p.reservationID, "chat_failed_before_output"); err != nil {
			log.Printf("chat release failed req=%s res=%s: %v", p.reqID, p.reservationID, err)
		}
		_ = s.emitLedgerEvent(ctx, contracts.LedgerEventRequest{
			IdempotencyKey: p.idempotencyKey + ":release",
			UserID:         p.userID,
			ReservationID:  p.reservationID,
			EventType:      "credits_released",
			Credits:        p.estimatedCredits,
			Payload:        map[string]any{"reason": "chat_failed_before_output", "surface": "chat"},
		})
	default:
		s.commitChat(ctx, p, p.estimatedCredits, "credits_committed_unknown", "unknown")
	}
}

func (s *server) commitChat(ctx context.Context, p settleParams, credits int64, eventType, basis string) {
	if err := s.commitReservation(ctx, p.userID, p.reservationID, credits); err != nil {
		log.Printf("chat commit failed req=%s res=%s: %v", p.reqID, p.reservationID, err)
		_ = s.emitLedgerEvent(ctx, contracts.LedgerEventRequest{
			IdempotencyKey: p.idempotencyKey + ":commit_failed",
			UserID:         p.userID,
			ReservationID:  p.reservationID,
			EventType:      "commit_failed",
			Credits:        credits,
			Payload:        map[string]any{"error": err.Error(), "surface": "chat"},
		})
		return
	}
	_ = s.emitLedgerEvent(ctx, contracts.LedgerEventRequest{
		IdempotencyKey: p.idempotencyKey + ":commit",
		UserID:         p.userID,
		ReservationID:  p.reservationID,
		EventType:      eventType,
		Credits:        credits,
		Payload: map[string]any{
			"estimated_credits": p.estimatedCredits,
			"usage_basis":       basis,
			"failed":            p.failed,
			"surface":           "chat",
		},
	})
}

func chatInput(req contracts.ChatRequest) ([]byte, error) {
	return json.Marshal(struct {
		Messages   []contracts.ChatMessage `json:"messages"`
		Tools      []contracts.ToolSchema  `json:"tools,omitempty"`
		ToolChoice *contracts.ToolChoice   `json:"tool_choice,omitempty"`
	}{req.Messages, req.Tools, req.ToolChoice})
}

func writeChatError(w http.ResponseWriter, status int, code, ref string) {
	body := map[string]string{"error": code}
	if ref != "" {
		body["ref"] = ref
	}
	httpx.WriteJSON(w, status, body)
}

// reservationErrorCode separates the two 429s the usage service can return:
// a per-user credit problem and platform-wide budget exhaustion are different
// operational events and the caller should be able to tell them apart.
func reservationErrorCode(err error) string {
	var upErr *httpx.UpstreamError
	if !errors.As(err, &upErr) {
		return "usage_service_unavailable"
	}
	if strings.Contains(upErr.Body, contracts.ErrPlatformBudgetExhausted) {
		return "platform_budget_exhausted"
	}
	switch upErr.StatusCode {
	case http.StatusPaymentRequired:
		return "insufficient_credits"
	case http.StatusTooManyRequests:
		return "rate_limited"
	default:
		return "usage_service_unavailable"
	}
}

func relayErrorCode(err error) string {
	var upErr *httpx.UpstreamError
	if !errors.As(err, &upErr) {
		return "provider_unavailable"
	}
	switch upErr.StatusCode {
	case http.StatusNotImplemented:
		return "provider_unsupported"
	case http.StatusTooManyRequests:
		return "rate_limited"
	case http.StatusUnauthorized, http.StatusBadRequest:
		return "provider_error"
	default:
		return "provider_unavailable"
	}
}
