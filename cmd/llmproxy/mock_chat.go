package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kh1011/creditproxy/pkg/contracts"
)

// The scripted mock replays a contract fixture so development and every later
// phase have a deterministic streaming backend with no API key and no spend.
//
// A caller picks a script with a directive in the last user message:
//
//	__script: multi-tool      replay pkg/contracts/testdata/chat/multi-tool.json
//	__script: tool-then-answer emit a tool round, then text after its result
//	__delay: 25               wait 25ms between frames
//	__script: hang            emit nothing and block until the context is cancelled
//
// With no directive the script is text-only.
const (
	defaultMockScript   = "text-only"
	hangMockScript      = "hang"
	runAwareMockScript  = "tool-then-answer"
	editorRewriteScript = "editor-rewrite"
	maxMockDelay        = 5 * time.Second
)

var (
	scriptDirective = regexp.MustCompile(`__script:\s*([a-z0-9-]+)`)
	delayDirective  = regexp.MustCompile(`__delay:\s*(\d+)`)
)

func (m *MockProvider) Chat(ctx context.Context, opts ChatOpts, emit func(contracts.ChatEvent) error) error {
	script, delay := parseMockDirectives(opts.Messages)
	if script == editorRewriteScript {
		events, err := editorRewriteEvents(opts.Messages)
		if err != nil {
			return err
		}
		return emitMockEvents(ctx, events, delay, emit)
	}
	if script == runAwareMockScript {
		script = "single-tool-round"
		if hasToolResult(opts.Messages) {
			script = "text-only"
		}
	}

	if script == hangMockScript {
		<-ctx.Done()
		return ctx.Err()
	}

	events, err := loadMockScript(script)
	if err != nil {
		return err
	}

	return emitMockEvents(ctx, events, delay, emit)
}

func emitMockEvents(ctx context.Context, events []contracts.ChatEvent, delay time.Duration, emit func(contracts.ChatEvent) error) error {
	for _, event := range events {
		if delay > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		} else if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := emit(event); err != nil {
			return err
		}
	}
	return nil
}

func editorRewriteEvents(messages []contracts.ChatMessage) ([]contracts.ChatEvent, error) {
	result := latestToolResult(messages)
	if result == nil {
		return mockToolCall("mock-read-editor", "read_current_editor", map[string]any{"selectionOnly": true}), nil
	}
	selection, _ := result["selection"].(map[string]any)
	chapterID, _ := result["chapter_id"].(string)
	original, _ := selection["text"].(string)
	if chapterID == "" || original == "" {
		return []contracts.ChatEvent{
			{Type: contracts.ChatEventTextDelta, Provider: "mock", Model: "mock-editor", Text: "Select text in the editor before asking for a revision."},
			{Type: contracts.ChatEventDone, FinishReason: contracts.FinishStop},
		}, nil
	}
	proposal := map[string]any{
		"chapterId":           chapterID,
		"baseRevision":        result["persisted_revision"],
		"baseDocumentVersion": result["document_version"],
		"summary":             "Tighten the selected sentence.",
		"operations": []any{map[string]any{
			"type":            "replace",
			"from":            selection["from"],
			"to":              selection["to"],
			"originalText":    original,
			"replacementText": "Tightened: " + original,
		}},
	}
	return mockToolCall("mock-propose-editor", "propose_editor_edit", proposal), nil
}

func latestToolResult(messages []contracts.ChatMessage) map[string]any {
	for i := len(messages) - 1; i >= 0; i-- {
		message := messages[i]
		if message.Role != contracts.RoleTool {
			continue
		}
		for _, part := range message.Parts {
			if part.Type != contracts.PartText {
				continue
			}
			var result map[string]any
			if json.Unmarshal([]byte(part.Text), &result) == nil {
				return result
			}
		}
	}
	return nil
}

func mockToolCall(id, name string, arguments any) []contracts.ChatEvent {
	raw, _ := json.Marshal(arguments)
	return []contracts.ChatEvent{
		{
			Type: contracts.ChatEventToolCallDelta, Provider: "mock", Model: "mock-editor",
			ToolCall: &contracts.ChatToolCallDelta{Index: 0, ToolCallID: id, Name: name},
		},
		{
			Type:     contracts.ChatEventToolCallDelta,
			ToolCall: &contracts.ChatToolCallDelta{Index: 0, ArgumentsDelta: string(raw)},
		},
		{
			Type: contracts.ChatEventUsage, Provider: "mock", Model: "mock-editor",
			Usage: &contracts.GenerateUsage{PromptTokens: 32, CompletionTokens: 16, TotalTokens: 48},
		},
		{Type: contracts.ChatEventDone, FinishReason: contracts.FinishToolCalls},
	}
}

func hasToolResult(messages []contracts.ChatMessage) bool {
	for _, message := range messages {
		if message.Role == contracts.RoleTool {
			return true
		}
	}
	return false
}

func parseMockDirectives(messages []contracts.ChatMessage) (string, time.Duration) {
	script, delay := defaultMockScript, time.Duration(0)

	text := ""
	for _, msg := range messages {
		if msg.Role != contracts.RoleUser {
			continue
		}
		for _, part := range msg.Parts {
			if part.Type == contracts.PartText {
				text = part.Text
			}
		}
	}
	if match := scriptDirective.FindStringSubmatch(text); match != nil {
		script = match[1]
	}
	if match := delayDirective.FindStringSubmatch(text); match != nil {
		ms, _ := strconv.Atoi(match[1])
		delay = time.Duration(ms) * time.Millisecond
		if delay > maxMockDelay {
			delay = maxMockDelay
		}
	}
	return script, delay
}

func loadMockScript(name string) ([]contracts.ChatEvent, error) {
	if name == "" || strings.ContainsAny(name, "/.") {
		return nil, &ProviderError{Provider: "mock", StatusCode: 400, Message: fmt.Sprintf("invalid script %q", name)}
	}
	raw, err := contracts.ChatFixtures.ReadFile(path.Join(contracts.ChatFixtureDir, name+".json"))
	if err != nil {
		return nil, &ProviderError{Provider: "mock", StatusCode: 404, Message: fmt.Sprintf("unknown script %q", name)}
	}
	var fixture struct {
		Stream []contracts.ChatEvent `json:"stream"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		return nil, fmt.Errorf("mock script %s: %w", name, err)
	}
	return fixture.Stream, nil
}
