package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kh1011/creditproxy/pkg/contracts"
)

func chatRequest(t *testing.T, text string, stream bool) *http.Request {
	t.Helper()
	body, err := json.Marshal(contracts.ChatRequest{
		Version:         contracts.ChatContractVersion,
		UserID:          "u1",
		Messages:        []contracts.ChatMessage{{Role: contracts.RoleUser, Parts: []contracts.ChatPart{{Type: contracts.PartText, Text: text}}}},
		MaxOutputTokens: 512,
		Stream:          stream,
	})
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewRequest(http.MethodPost, "/v1/chat", strings.NewReader(string(body)))
}

func TestChatStreamsScriptedToolRound(t *testing.T) {
	s := &server{provider: &MockProvider{}, mock: &MockProvider{}}
	rr := httptest.NewRecorder()
	s.handleChat(rr, chatRequest(t, "__script: single-tool-round", true))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}

	var types []string
	for _, line := range strings.Split(rr.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event contracts.ChatEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatalf("unparseable frame %q: %v", line, err)
		}
		types = append(types, string(event.Type))
	}
	want := []string{"tool_call_delta", "tool_call_delta", "text_delta", "usage", "done"}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Errorf("event types = %v, want %v", types, want)
	}
}

func TestChatBuffersToolCallsWhenNotStreaming(t *testing.T) {
	s := &server{provider: &MockProvider{}, mock: &MockProvider{}}
	rr := httptest.NewRecorder()
	s.handleChat(rr, chatRequest(t, "__script: single-tool-round", false))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	var resp contracts.ChatResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Usage.TotalTokens != 537 {
		t.Errorf("usage total = %d, want 537", resp.Usage.TotalTokens)
	}
	if resp.FinishReason != contracts.FinishToolCalls {
		t.Errorf("finish reason = %q, want tool_calls", resp.FinishReason)
	}
	var sawToolCall bool
	for _, part := range resp.Parts {
		if part.Type == contracts.PartToolCall && part.Name == "get_story_overview" {
			sawToolCall = true
		}
	}
	if !sawToolCall {
		t.Errorf("aggregated parts lost the tool call: %+v", resp.Parts)
	}
}

func TestChatRejectsUnknownContractVersion(t *testing.T) {
	s := &server{provider: &MockProvider{}, mock: &MockProvider{}}
	rr := httptest.NewRecorder()
	rr2 := httptest.NewRequest(http.MethodPost, "/v1/chat", strings.NewReader(`{"version":2,"user_id":"u1","messages":[{"role":"user","parts":[{"type":"text","text":"hi"}]}],"max_output_tokens":10}`))
	s.handleChat(rr, rr2)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
}
