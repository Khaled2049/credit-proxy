package main

import (
	"context"
	"testing"

	"github.com/kh1011/creditproxy/pkg/contracts"
)

func collectMockEvents(t *testing.T, messages []contracts.ChatMessage) []contracts.ChatEvent {
	t.Helper()
	var events []contracts.ChatEvent
	err := (&MockProvider{}).Chat(context.Background(), ChatOpts{Messages: messages}, func(event contracts.ChatEvent) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func TestRunAwareMockUsesToolResultAsTheStepBoundary(t *testing.T) {
	prompt := contracts.ChatMessage{
		Role:  contracts.RoleUser,
		Parts: []contracts.ChatPart{{Type: contracts.PartText, Text: "How many chapters? __script: tool-then-answer"}},
	}
	first := collectMockEvents(t, []contracts.ChatMessage{prompt})
	if got := first[len(first)-1].FinishReason; got != contracts.FinishToolCalls {
		t.Fatalf("first finish reason = %q, want tool_calls", got)
	}
	if first[0].Type != contracts.ChatEventToolCallDelta {
		t.Fatalf("first event = %q, want tool_call_delta", first[0].Type)
	}

	toolResult := contracts.ChatMessage{
		Role:       contracts.RoleTool,
		ToolCallID: "call-1",
		Parts:      []contracts.ChatPart{{Type: contracts.PartText, Text: `{"title":"Saltmarsh","chapter_count":3}`}},
	}
	second := collectMockEvents(t, []contracts.ChatMessage{prompt, toolResult})
	if got := second[len(second)-1].FinishReason; got != contracts.FinishStop {
		t.Fatalf("second finish reason = %q, want stop", got)
	}
	if second[0].Type != contracts.ChatEventTextDelta {
		t.Fatalf("second event = %q, want text_delta", second[0].Type)
	}
}

func TestRepeatingToolFixtureRemainsAvailableForMaxSteps(t *testing.T) {
	prompt := contracts.ChatMessage{
		Role:  contracts.RoleUser,
		Parts: []contracts.ChatPart{{Type: contracts.PartText, Text: "__script: single-tool-round"}},
	}
	toolResult := contracts.ChatMessage{Role: contracts.RoleTool, ToolCallID: "call-1"}
	events := collectMockEvents(t, []contracts.ChatMessage{prompt, toolResult})
	if got := events[len(events)-1].FinishReason; got != contracts.FinishToolCalls {
		t.Fatalf("repeating fixture finish reason = %q, want tool_calls", got)
	}
}
