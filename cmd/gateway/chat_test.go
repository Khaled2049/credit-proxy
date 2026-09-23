package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	authn "github.com/kh1011/creditproxy/pkg/auth"
	"github.com/kh1011/creditproxy/pkg/contracts"
	"github.com/kh1011/creditproxy/pkg/httpx"
)

type chatStub struct {
	mu           sync.Mutex
	reserved     int64
	committed    int64
	commits      int
	releases     int
	ledgerEvents []string

	reserveStatus  int
	reserveBody    string
	llmStatus      int
	llmEvents      []string
	holdAfterFirst bool
}

func (c *chatStub) snapshot() (reserved, committed int64, commits, releases int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reserved, c.committed, c.commits, c.releases
}

func (c *chatStub) hasLedgerEvent(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, event := range c.ledgerEvents {
		if event == name {
			return true
		}
	}
	return false
}

func newChatStub(stub *chatStub) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/reservations":
			if stub.reserveStatus != 0 {
				http.Error(w, stub.reserveBody, stub.reserveStatus)
				return
			}
			var req contracts.ReservationRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			stub.mu.Lock()
			stub.reserved = req.EstimatedCredits
			stub.mu.Unlock()
			httpx.WriteJSON(w, http.StatusOK, contracts.ReservationResponse{
				ReservationID: req.ReservationID, UserID: req.UserID,
				Reserved: req.EstimatedCredits, Status: "reserved",
			})

		case r.URL.Path == "/v1/chat":
			if stub.llmStatus != 0 {
				http.Error(w, "upstream refused", stub.llmStatus)
				return
			}
			var req contracts.ChatRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if !req.Stream {
				httpx.WriteJSON(w, http.StatusOK, contracts.ChatResponse{
					Provider: "mock", Model: "mock",
					Parts:        []contracts.ChatPart{{Type: contracts.PartText, Text: "hi"}},
					Usage:        contracts.GenerateUsage{PromptTokens: 200, CompletionTokens: 250, TotalTokens: 450},
					FinishReason: contracts.FinishStop,
				})
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher := w.(http.Flusher)
			for i, event := range stub.llmEvents {
				fmt.Fprintf(w, "data: %s\n\n", event)
				flusher.Flush()
				// Hold the stream open so a client that hangs up aborts the
				// read mid-flight instead of racing a clean finish.
				if i == 0 && stub.holdAfterFirst {
					time.Sleep(500 * time.Millisecond)
				}
			}

		case strings.HasSuffix(r.URL.Path, "/commit"):
			var req contracts.CommitReservationRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			stub.mu.Lock()
			stub.committed = req.ActualCredits
			stub.commits++
			stub.mu.Unlock()
			httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "committed"})

		case strings.HasSuffix(r.URL.Path, "/release"):
			stub.mu.Lock()
			stub.releases++
			stub.mu.Unlock()
			httpx.WriteJSON(w, http.StatusOK, map[string]any{"status": "released"})

		case r.URL.Path == "/v1/events":
			var req contracts.LedgerEventRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			stub.mu.Lock()
			stub.ledgerEvents = append(stub.ledgerEvents, req.EventType)
			stub.mu.Unlock()
			httpx.WriteJSON(w, http.StatusOK, map[string]any{})

		default:
			http.NotFound(w, r)
		}
	}))
}

func newChatServer(url string) *server {
	return &server{
		usageURL:                 url,
		llmURL:                   url,
		ledgerURL:                url,
		client:                   httpx.NewHTTPClient(5 * time.Second),
		streamClient:             httpx.NewStreamingHTTPClient(5 * time.Second),
		verifier:                 authn.NewVerifier(authn.Config{Mode: authn.ModeDev}),
		maxOutputTokens:          8192,
		maxPromptChars:           64000,
		maxChatInput:             262144,
		tokensPerCredit:          100,
		rateLimiter:              newUserRateLimiter(10),
		chatRateLimiter:          newUserRateLimiter(60),
		platformInferenceEnabled: true,
	}
}

func chatRequestBody(t *testing.T) string {
	t.Helper()
	body, err := json.Marshal(contracts.ChatRequest{
		Version:         contracts.ChatContractVersion,
		UserID:          "user123",
		Messages:        []contracts.ChatMessage{{Role: contracts.RoleUser, Parts: []contracts.ChatPart{{Type: contracts.PartText, Text: "hello"}}}},
		MaxOutputTokens: 1000,
		Stream:          true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

const (
	textDeltaEvent = `{"type":"text_delta","text":"hi"}`
	// 450 total tokens at TOKENS_PER_CREDIT=100 reconciles to 5 credits.
	usageEvent = `{"type":"usage","usage":{"prompt_tokens":200,"completion_tokens":250,"total_tokens":450}}`
	doneEvent  = `{"type":"done","finish_reason":"stop"}`
	errorEvent = `{"type":"error","error":{"code":"provider_unavailable","message":"upstream died"}}`
)

// The billing outcomes of a finished stream. The unknown-usage rows are the
// conservative policy: when the platform cannot know what it was charged, it
// keeps the hold it already sized.
func TestChatSettlement(t *testing.T) {
	cases := []struct {
		name          string
		llmStatus     int
		llmEvents     []string
		wantCommit    int64
		wantCommitted bool
		wantReleased  bool
		wantLedger    string
	}{
		{
			name:          "usage reported commits real usage",
			llmEvents:     []string{textDeltaEvent, usageEvent, doneEvent},
			wantCommit:    5,
			wantCommitted: true,
			wantLedger:    "credits_committed",
		},
		{
			name:         "refused before any output releases the hold",
			llmStatus:    http.StatusBadGateway,
			wantReleased: true,
			wantLedger:   "credits_released",
		},
		{
			name:          "error after output commits the full hold",
			llmEvents:     []string{textDeltaEvent, errorEvent},
			wantCommitted: true,
			wantLedger:    "credits_committed_unknown",
		},
		{
			name:          "clean stream with no usage commits the full hold",
			llmEvents:     []string{textDeltaEvent, doneEvent},
			wantCommitted: true,
			wantLedger:    "credits_committed_unknown",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &chatStub{llmStatus: tc.llmStatus, llmEvents: tc.llmEvents}
			srv := newChatStub(stub)
			defer srv.Close()
			s := newChatServer(srv.URL)

			rr := httptest.NewRecorder()
			s.handleChat(rr, httptest.NewRequest(http.MethodPost, "/v1/chat", strings.NewReader(chatRequestBody(t))))

			reserved, committed, commits, releases := stub.snapshot()
			if reserved <= 0 {
				t.Fatalf("expected a reservation, got %d", reserved)
			}
			if tc.wantReleased && releases != 1 {
				t.Errorf("releases = %d, want 1", releases)
			}
			if !tc.wantReleased && releases != 0 {
				t.Errorf("releases = %d, want 0", releases)
			}
			if tc.wantCommitted && commits != 1 {
				t.Errorf("commits = %d, want 1", commits)
			}
			if !tc.wantCommitted && commits != 0 {
				t.Errorf("commits = %d, want 0", commits)
			}
			if tc.wantCommitted {
				want := tc.wantCommit
				if want == 0 {
					want = reserved
				}
				if committed != want {
					t.Errorf("committed = %d, want %d", committed, want)
				}
			}
			if !stub.hasLedgerEvent(tc.wantLedger) {
				t.Errorf("missing ledger event %q", tc.wantLedger)
			}
		})
	}
}

// cancelOnWrite models a client hanging up: the request context is cancelled
// the moment the first relayed frame is written to it.
type cancelOnWrite struct {
	*httptest.ResponseRecorder
	cancel context.CancelFunc
	once   sync.Once
}

func (c *cancelOnWrite) Write(b []byte) (int, error) {
	n, err := c.ResponseRecorder.Write(b)
	c.once.Do(c.cancel)
	return n, err
}

func (c *cancelOnWrite) Flush() {}

// A client that hangs up mid-stream leaves usage unknown, so the hold stands.
// The settlement calls must also survive the cancelled request context.
func TestChatClientDisconnectCommitsFullHold(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stub := &chatStub{
		llmEvents:      []string{textDeltaEvent, usageEvent, doneEvent},
		holdAfterFirst: true,
	}
	srv := newChatStub(stub)
	defer srv.Close()
	s := newChatServer(srv.URL)

	rr := &cancelOnWrite{ResponseRecorder: httptest.NewRecorder(), cancel: cancel}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat", strings.NewReader(chatRequestBody(t))).WithContext(ctx)
	s.handleChat(rr, req)

	reserved, committed, commits, releases := stub.snapshot()
	if commits != 1 || releases != 0 {
		t.Fatalf("commits = %d, releases = %d, want 1 and 0", commits, releases)
	}
	if committed != reserved {
		t.Errorf("committed = %d, want the full hold %d", committed, reserved)
	}
	if !stub.hasLedgerEvent("credits_committed_unknown") {
		t.Error("expected a credits_committed_unknown ledger event")
	}
}

func TestChatPlatformBudgetExhausted(t *testing.T) {
	stub := &chatStub{reserveStatus: http.StatusTooManyRequests, reserveBody: contracts.ErrPlatformBudgetExhausted}
	srv := newChatStub(stub)
	defer srv.Close()
	s := newChatServer(srv.URL)

	rr := httptest.NewRecorder()
	s.handleChat(rr, httptest.NewRequest(http.MethodPost, "/v1/chat", strings.NewReader(chatRequestBody(t))))

	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "platform_budget_exhausted") {
		t.Errorf("body = %s, want a platform_budget_exhausted code", rr.Body.String())
	}
	if _, _, commits, releases := stub.snapshot(); commits != 0 || releases != 0 {
		t.Errorf("a refused reservation must not settle: commits = %d, releases = %d", commits, releases)
	}
}

func TestChatKillSwitchBlocksPlatformOnly(t *testing.T) {
	stub := &chatStub{llmEvents: []string{textDeltaEvent, usageEvent, doneEvent}}
	srv := newChatStub(stub)
	defer srv.Close()
	s := newChatServer(srv.URL)
	s.platformInferenceEnabled = false

	rr := httptest.NewRecorder()
	s.handleChat(rr, httptest.NewRequest(http.MethodPost, "/v1/chat", strings.NewReader(chatRequestBody(t))))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("platform request: status = %d, want 503", rr.Code)
	}
	if reserved, _, _, _ := stub.snapshot(); reserved != 0 {
		t.Error("kill switch must refuse before reserving")
	}

	body, _ := json.Marshal(contracts.ChatRequest{
		Version:         contracts.ChatContractVersion,
		UserID:          "user123",
		Messages:        []contracts.ChatMessage{{Role: contracts.RoleUser, Parts: []contracts.ChatPart{{Type: contracts.PartText, Text: "hello"}}}},
		MaxOutputTokens: 1000,
		Stream:          true,
		BYOKProvider:    "gemini",
		BYOKApiKey:      "user-key",
	})
	rr = httptest.NewRecorder()
	s.handleChat(rr, httptest.NewRequest(http.MethodPost, "/v1/chat", strings.NewReader(string(body))))
	if rr.Code != http.StatusOK {
		t.Fatalf("byok request: status = %d, want 200 (kill switch is platform-only)", rr.Code)
	}
	if reserved, _, commits, _ := stub.snapshot(); reserved != 0 || commits != 0 {
		t.Error("byok must not reserve or commit platform credits")
	}

	var mockReq contracts.ChatRequest
	if err := json.Unmarshal([]byte(chatRequestBody(t)), &mockReq); err != nil {
		t.Fatal(err)
	}
	mockReq.ForceMock = true
	body, _ = json.Marshal(mockReq)
	rr = httptest.NewRecorder()
	s.handleChat(rr, httptest.NewRequest(http.MethodPost, "/v1/chat", strings.NewReader(string(body))))
	if rr.Code != http.StatusOK {
		t.Fatalf("forced mock request: status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	if reserved, _, commits, _ := stub.snapshot(); reserved != 0 || commits != 0 {
		t.Error("forced mock must not reserve or commit platform credits")
	}
	if !stub.hasLedgerEvent("mock_chat") {
		t.Error("forced mock should emit a mock_chat audit event")
	}
}

func TestChatBufferedCommitsRealUsage(t *testing.T) {
	stub := &chatStub{}
	srv := newChatStub(stub)
	defer srv.Close()
	s := newChatServer(srv.URL)

	body, _ := json.Marshal(contracts.ChatRequest{
		Version:         contracts.ChatContractVersion,
		UserID:          "user123",
		Messages:        []contracts.ChatMessage{{Role: contracts.RoleUser, Parts: []contracts.ChatPart{{Type: contracts.PartText, Text: "hello"}}}},
		MaxOutputTokens: 1000,
	})
	rr := httptest.NewRecorder()
	s.handleChat(rr, httptest.NewRequest(http.MethodPost, "/v1/chat", strings.NewReader(string(body))))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	var resp contracts.ChatResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Credits != 5 {
		t.Errorf("credits = %d, want 5 (450 tokens / 100)", resp.Credits)
	}
	if _, committed, commits, releases := stub.snapshot(); commits != 1 || releases != 0 || committed != 5 {
		t.Errorf("commits = %d, releases = %d, committed = %d; want 1, 0, 5", commits, releases, committed)
	}
}

func toolHeavyChatRequest(t *testing.T, schemaBytes int) (string, contracts.ChatRequest) {
	t.Helper()
	req := contracts.ChatRequest{
		Version: contracts.ChatContractVersion,
		UserID:  "user123",
		Messages: []contracts.ChatMessage{
			{Role: contracts.RoleUser, Parts: []contracts.ChatPart{{Type: contracts.PartText, Text: "hi"}}},
			{Role: contracts.RoleAssistant, Parts: []contracts.ChatPart{{
				Type: contracts.PartToolCall, ToolCallID: "call_1", Name: "search_story",
				Arguments: map[string]any{"query": strings.Repeat("q", 4000)},
			}}},
			{Role: contracts.RoleTool, ToolCallID: "call_1", Parts: []contracts.ChatPart{{Type: contracts.PartText, Text: "ok"}}},
		},
		Tools: []contracts.ToolSchema{{
			Name:        "search_story",
			Description: strings.Repeat("d", schemaBytes),
			Parameters:  map[string]any{"type": "object"},
		}},
		MaxOutputTokens: 1000,
		Stream:          true,
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return string(body), req
}

func TestChatReservationCoversToolsAndToolCallArguments(t *testing.T) {
	stub := &chatStub{llmEvents: []string{textDeltaEvent, usageEvent, doneEvent}}
	srv := newChatStub(stub)
	defer srv.Close()
	s := newChatServer(srv.URL)

	body, req := toolHeavyChatRequest(t, 20000)
	rr := httptest.NewRecorder()
	s.handleChat(rr, httptest.NewRequest(http.MethodPost, "/v1/chat", strings.NewReader(body)))

	input, err := chatInput(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(input) < 24000 {
		t.Fatalf("serialized input = %d bytes, expected tool schema and arguments to be included", len(input))
	}
	want := (int64(len(input)) + 32 + 1000 + 99) / 100
	reserved, _, _, _ := stub.snapshot()
	if reserved != want {
		t.Fatalf("reserved = %d credits, want %d", reserved, want)
	}
}

func TestChatRejectsOversizedSerializedInput(t *testing.T) {
	stub := &chatStub{}
	srv := newChatStub(stub)
	defer srv.Close()
	s := newChatServer(srv.URL)
	s.maxChatInput = 10000

	body, _ := toolHeavyChatRequest(t, 20000)
	rr := httptest.NewRecorder()
	s.handleChat(rr, httptest.NewRequest(http.MethodPost, "/v1/chat", strings.NewReader(body)))

	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "prompt_too_large") {
		t.Fatalf("status = %d body = %s, want 400 prompt_too_large", rr.Code, rr.Body.String())
	}
	if reserved, _, _, _ := stub.snapshot(); reserved != 0 {
		t.Fatalf("reserved = %d, want no reservation", reserved)
	}
}
