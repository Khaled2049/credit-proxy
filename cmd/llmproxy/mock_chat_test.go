package main

import (
	"context"
	"encoding/json"
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

func TestEditorRewriteMockReadsSelectionThenProposesLinkedEdit(t *testing.T) {
	prompt := contracts.ChatMessage{
		Role:  contracts.RoleUser,
		Parts: []contracts.ChatPart{{Type: contracts.PartText, Text: "Tighten this. __script: editor-rewrite"}},
	}
	first := collectMockEvents(t, []contracts.ChatMessage{prompt})
	if got := first[0].ToolCall.Name; got != "read_current_editor" {
		t.Fatalf("first tool = %q, want read_current_editor", got)
	}

	toolResult := contracts.ChatMessage{
		Role:       contracts.RoleTool,
		ToolCallID: "mock-read-editor",
		Parts:      []contracts.ChatPart{{Type: contracts.PartText, Text: `{"available":true,"chapter_id":"chapter-1","persisted_revision":4,"document_version":9,"selection":{"from":7,"to":12,"text":"brave"}}`}},
	}
	second := collectMockEvents(t, []contracts.ChatMessage{prompt, toolResult})
	if got := second[0].ToolCall.Name; got != "propose_editor_edit" {
		t.Fatalf("second tool = %q, want propose_editor_edit", got)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(second[1].ToolCall.ArgumentsDelta), &args); err != nil {
		t.Fatal(err)
	}
	if args["chapterId"] != "chapter-1" || args["baseRevision"] != float64(4) {
		t.Fatalf("proposal lost editor linkage: %#v", args)
	}
	operations := args["operations"].([]any)
	replace := operations[0].(map[string]any)
	if replace["originalText"] != "brave" {
		t.Fatalf("original text = %#v, want brave", replace["originalText"])
	}
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
