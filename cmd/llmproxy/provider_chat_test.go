package main

import (
	"context"
	"strings"
	"testing"

	"github.com/kh1011/creditproxy/pkg/contracts"
)

func collect(t *testing.T, run func(emit func(contracts.ChatEvent) error) error) []contracts.ChatEvent {
	t.Helper()
	var events []contracts.ChatEvent
	if err := run(func(event contracts.ChatEvent) error {
		events = append(events, event)
		return nil
	}); err != nil {
		t.Fatalf("chat: %v", err)
	}
	return events
}

func TestOllamaChatNormalizesStream(t *testing.T) {
	body := strings.Join([]string{
		`{"model":"llama3","message":{"role":"assistant","content":"Sal"},"done":false}`,
		`{"model":"llama3","message":{"role":"assistant","content":"tmarsh"},"done":false}`,
		`{"model":"llama3","message":{"role":"assistant","content":"","tool_calls":[{"function":{"name":"get_story_overview","arguments":{"a":1}}}]},"done":false}`,
		`{"model":"llama3","done":true,"done_reason":"stop","prompt_eval_count":120,"eval_count":8}`,
	}, "\n")
	provider := &OllamaProvider{baseURL: "http://localhost:11434", model: "llama3", client: fixedResponseClient(200, body)}

	events := collect(t, func(emit func(contracts.ChatEvent) error) error {
		return provider.Chat(context.Background(), ChatOpts{MaxOutputTokens: 128}, emit)
	})

	var types []string
	for _, event := range events {
		types = append(types, string(event.Type))
	}
	want := "text_delta,text_delta,tool_call_delta,usage,done"
	if got := strings.Join(types, ","); got != want {
		t.Fatalf("events = %s, want %s", got, want)
	}
	if call := events[2].ToolCall; call == nil || call.Name != "get_story_overview" || call.ArgumentsDelta != `{"a":1}` {
		t.Errorf("tool call not normalized: %+v", events[2].ToolCall)
	}
	if events[2].ToolCall.ToolCallID == "" {
		t.Error("ollama sends no call id, so one must be minted")
	}
	if usage := events[3].Usage; usage == nil || usage.TotalTokens != 128 {
		t.Errorf("usage = %+v, want 128 total", events[3].Usage)
	}
	if events[4].FinishReason != contracts.FinishToolCalls {
		t.Errorf("finish = %q, want tool_calls", events[4].FinishReason)
	}
}

func TestOllamaChatRequiredToolNotProduced(t *testing.T) {
	body := `{"model":"llama3","message":{"role":"assistant","content":"just prose"},"done":true,"done_reason":"stop"}`
	provider := &OllamaProvider{baseURL: "http://localhost:11434", model: "llama3", client: fixedResponseClient(200, body)}

	err := provider.Chat(context.Background(), ChatOpts{
		MaxOutputTokens: 128,
		ToolChoice:      &contracts.ToolChoice{Mode: contracts.ToolChoiceRequired},
	}, func(contracts.ChatEvent) error { return nil })
	if err == nil {
		t.Fatal("a model that ignores a required tool call must fail, not answer")
	}
}

func TestGeminiChatNormalizesStream(t *testing.T) {
	body := strings.Join([]string{
		`data: {"candidates":[{"content":{"parts":[{"text":"Saltmarsh"}]}}],"modelVersion":"gemini-2.5-flash-lite"}`,
		"",
		`data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"search_story","args":{"query":"Mina"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":300,"candidatesTokenCount":12,"totalTokenCount":312}}`,
		"",
	}, "\n")
	provider := &GeminiProvider{apiKey: "k", model: "gemini-2.5-flash-lite", client: fixedResponseClient(200, body), userAgent: "test"}

	events := collect(t, func(emit func(contracts.ChatEvent) error) error {
		return provider.Chat(context.Background(), ChatOpts{MaxOutputTokens: 128}, emit)
	})

	var types []string
	for _, event := range events {
		types = append(types, string(event.Type))
	}
	if got, want := strings.Join(types, ","), "text_delta,tool_call_delta,usage,done"; got != want {
		t.Fatalf("events = %s, want %s", got, want)
	}
	if call := events[1].ToolCall; call == nil || call.Name != "search_story" || call.ArgumentsDelta != `{"query":"Mina"}` {
		t.Errorf("tool call not normalized: %+v", events[1].ToolCall)
	}
	if usage := events[2].Usage; usage == nil || usage.TotalTokens != 312 {
		t.Errorf("usage = %+v, want 312 total", events[2].Usage)
	}
	if events[3].FinishReason != contracts.FinishToolCalls {
		t.Errorf("finish = %q, want tool_calls", events[3].FinishReason)
	}
}

// Gemini keys a function response by name and has no call id, so the name has
// to survive the round trip through the neutral message shape.
func TestGeminiContentsRecoversFunctionName(t *testing.T) {
	contents, system := geminiContents([]contracts.ChatMessage{
		{Role: contracts.RoleSystem, Parts: []contracts.ChatPart{{Type: contracts.PartText, Text: "be brief"}}},
		{Role: contracts.RoleUser, Parts: []contracts.ChatPart{{Type: contracts.PartText, Text: "how many chapters?"}}},
		{Role: contracts.RoleAssistant, Parts: []contracts.ChatPart{{
			Type: contracts.PartToolCall, ToolCallID: "call-1", Name: "get_story_overview", Arguments: map[string]any{},
		}}},
		{Role: contracts.RoleTool, ToolCallID: "call-1", Parts: []contracts.ChatPart{{Type: contracts.PartText, Text: "3"}}},
	})

	if system == nil {
		t.Fatal("system message must become systemInstruction")
	}
	if len(contents) != 3 {
		t.Fatalf("contents = %d, want 3 (system is hoisted out)", len(contents))
	}
	parts := contents[2]["parts"].([]map[string]any)
	response := parts[0]["functionResponse"].(map[string]any)
	if response["name"] != "get_story_overview" {
		t.Errorf("functionResponse name = %v, want get_story_overview", response["name"])
	}
	if contents[1]["role"] != "model" {
		t.Errorf("assistant role = %v, want model", contents[1]["role"])
	}
}

func TestValidateLocalUnmetered(t *testing.T) {
	ollama := func(base string) Provider {
		return &OllamaProvider{baseURL: base, model: "llama3"}
	}
	cases := []struct {
		name     string
		provider Provider
		raw      string
		wantErr  bool
	}{
		{"off by default", &GeminiProvider{model: "x"}, "false", false},
		{"loopback ok", ollama("http://localhost:11434"), "true", false},
		{"loopback ip ok", ollama("http://127.0.0.1:11434"), "true", false},
		{"docker service name ok", ollama("http://ollama:11434"), "true", false},
		{"private range ok", ollama("http://192.168.1.20:11434"), "true", false},
		{"docker host gateway ok", ollama("http://host.docker.internal:11434"), "true", false},
		{"hosted provider refused", &GeminiProvider{model: "x"}, "true", true},
		{"public ollama refused", ollama("https://ollama.example.com"), "true", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateLocalUnmetered(tc.provider, tc.raw)
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr = %t", err, tc.wantErr)
			}
		})
	}
}
