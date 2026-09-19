package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/kh1011/creditproxy/pkg/contracts"
	"github.com/kh1011/creditproxy/pkg/ids"
)

// Gemini streams SSE and sends a function call as one complete object, so a
// tool call becomes a single tool_call_delta rather than argument fragments.
// It also has no notion of a tool-call id, so one is minted per call and
// carried back through the message history.

type geminiPart struct {
	Text         string `json:"text,omitempty"`
	FunctionCall *struct {
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
	} `json:"functionCall,omitempty"`
	ThoughtSignature string `json:"thoughtSignature,omitempty"`
}

type geminiStreamFrame struct {
	Candidates []struct {
		Content struct {
			Parts []geminiPart `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount     int64 `json:"promptTokenCount"`
		CandidatesTokenCount int64 `json:"candidatesTokenCount"`
		TotalTokenCount      int64 `json:"totalTokenCount"`
	} `json:"usageMetadata"`
	ModelVersion string `json:"modelVersion"`
}

func (g *GeminiProvider) Chat(ctx context.Context, opts ChatOpts, emit func(contracts.ChatEvent) error) error {
	endpoint := "https://generativelanguage.googleapis.com/v1beta/models/" +
		url.PathEscape(g.model) + ":streamGenerateContent?alt=sse"

	contents, systemInstruction := geminiContents(opts.Messages)
	body := map[string]any{
		"contents": contents,
		"generationConfig": map[string]any{
			"maxOutputTokens": opts.MaxOutputTokens,
			"temperature":     opts.Temperature,
		},
	}
	if systemInstruction != nil {
		body["systemInstruction"] = systemInstruction
	}
	if declarations := geminiTools(opts.Tools); len(declarations) > 0 {
		body["tools"] = []map[string]any{{"functionDeclarations": declarations}}
		if config := geminiToolConfig(opts.ToolChoice); config != nil {
			body["toolConfig"] = config
		}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", g.userAgent)
	req.Header.Set("x-goog-api-key", g.apiKey)

	resp, err := g.client.Do(req)
	if err != nil {
		return &ProviderError{Provider: "gemini", Message: fmt.Sprintf("unreachable: %v", err)}
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &ProviderError{Provider: "gemini", StatusCode: resp.StatusCode, Message: string(msg)}
	}

	var (
		toolIndex    int
		sawTool      bool
		finishReason string
		usage        contracts.GenerateUsage
		model        = g.model
	)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		raw := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if raw == "" || raw == "[DONE]" {
			continue
		}
		var frame geminiStreamFrame
		if err := json.Unmarshal([]byte(raw), &frame); err != nil {
			return fmt.Errorf("gemini decode: %w", err)
		}
		if frame.ModelVersion != "" {
			model = frame.ModelVersion
		}
		if frame.UsageMetadata.TotalTokenCount > 0 {
			usage = contracts.GenerateUsage{
				PromptTokens:     frame.UsageMetadata.PromptTokenCount,
				CompletionTokens: frame.UsageMetadata.CandidatesTokenCount,
				TotalTokens:      frame.UsageMetadata.TotalTokenCount,
			}
		}
		for _, candidate := range frame.Candidates {
			if candidate.FinishReason != "" {
				finishReason = candidate.FinishReason
			}
			for _, part := range candidate.Content.Parts {
				if part.Text != "" {
					if err := emit(contracts.ChatEvent{
						Type:     contracts.ChatEventTextDelta,
						Provider: "gemini",
						Model:    model,
						Text:     part.Text,
					}); err != nil {
						return err
					}
				}
				if part.FunctionCall != nil {
					sawTool = true
					if err := emit(contracts.ChatEvent{
						Type:     contracts.ChatEventToolCallDelta,
						Provider: "gemini",
						Model:    model,
						ToolCall: &contracts.ChatToolCallDelta{
							Index:          toolIndex,
							ToolCallID:     ids.New("call"),
							Name:           part.FunctionCall.Name,
							ArgumentsDelta: string(part.FunctionCall.Args),
							ProviderMeta:   geminiProviderMeta(part.ThoughtSignature),
						},
					}); err != nil {
						return err
					}
					toolIndex++
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}

	if opts.ToolChoice != nil && opts.ToolChoice.Mode == contracts.ToolChoiceRequired && !sawTool {
		return &ProviderError{
			Provider:   "gemini",
			StatusCode: http.StatusNotFound,
			Message:    "model " + g.model + " did not produce a required tool call",
		}
	}

	if usage.TotalTokens > 0 {
		if err := emit(contracts.ChatEvent{
			Type:     contracts.ChatEventUsage,
			Provider: "gemini",
			Model:    model,
			Usage:    &usage,
		}); err != nil {
			return err
		}
	}
	return emit(contracts.ChatEvent{
		Type:         contracts.ChatEventDone,
		Provider:     "gemini",
		Model:        model,
		FinishReason: geminiFinishReason(finishReason, sawTool),
	})
}

func geminiFinishReason(reason string, sawTool bool) contracts.FinishReason {
	switch {
	case reason == "MAX_TOKENS":
		return contracts.FinishLength
	case sawTool:
		return contracts.FinishToolCalls
	default:
		return contracts.FinishStop
	}
}

// geminiContents converts the neutral message list into Gemini's shape:
// "model" instead of "assistant", system messages hoisted into
// systemInstruction, and tool results as functionResponse parts. Gemini's
// functionResponse is keyed by function *name*, not call id, so the name is
// recovered from the assistant turn that requested it.
func geminiContents(messages []contracts.ChatMessage) ([]map[string]any, map[string]any) {
	var (
		contents          []map[string]any
		systemInstruction map[string]any
		callNames         = map[string]string{}
	)
	for _, message := range messages {
		switch message.Role {
		case contracts.RoleSystem:
			var text string
			for _, part := range message.Parts {
				text += part.Text
			}
			if text != "" {
				systemInstruction = map[string]any{"parts": []map[string]any{{"text": text}}}
			}

		case contracts.RoleTool:
			var text string
			for _, part := range message.Parts {
				text += part.Text
			}
			name := callNames[message.ToolCallID]
			if name == "" {
				name = "tool"
			}
			contents = append(contents, map[string]any{
				"role": "user",
				"parts": []map[string]any{{
					"functionResponse": map[string]any{
						"name":     name,
						"response": map[string]any{"content": text},
					},
				}},
			})

		default:
			role := "user"
			if message.Role == contracts.RoleAssistant {
				role = "model"
			}
			parts := make([]map[string]any, 0, len(message.Parts))
			for _, part := range message.Parts {
				switch part.Type {
				case contracts.PartText:
					if part.Text != "" {
						parts = append(parts, map[string]any{"text": part.Text})
					}
				case contracts.PartToolCall:
					callNames[part.ToolCallID] = part.Name
					call := map[string]any{
						"functionCall": map[string]any{
							"name": part.Name,
							"args": part.Arguments,
						},
					}
					if signature := geminiThoughtSignature(part.ProviderMeta); signature != "" {
						call["thoughtSignature"] = signature
					}
					parts = append(parts, call)
				}
			}
			if len(parts) > 0 {
				contents = append(contents, map[string]any{"role": role, "parts": parts})
			}
		}
	}
	return contents, systemInstruction
}

func geminiTools(tools []contracts.ToolSchema) []map[string]any {
	if len(tools) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		declaration := map[string]any{"name": tool.Name}
		if tool.Description != "" {
			declaration["description"] = tool.Description
		}
		if tool.Parameters != nil {
			declaration["parameters"] = geminiParameters(tool.Parameters)
		}
		out = append(out, declaration)
	}
	return out
}

var geminiSchemaFields = map[string]bool{
	"type":             true,
	"format":           true,
	"title":            true,
	"description":      true,
	"nullable":         true,
	"enum":             true,
	"items":            true,
	"minItems":         true,
	"maxItems":         true,
	"properties":       true,
	"required":         true,
	"minProperties":    true,
	"maxProperties":    true,
	"propertyOrdering": true,
	"minLength":        true,
	"maxLength":        true,
	"pattern":          true,
	"minimum":          true,
	"maximum":          true,
	"anyOf":            true,
	"default":          true,
	"example":          true,
}

const geminiMaxRefDepth = 8

func geminiParameters(parameters any) any {
	root, ok := parameters.(map[string]any)
	if !ok {
		return parameters
	}
	defs, _ := root["$defs"].(map[string]any)
	return geminiSchema(root, defs, 0)
}

func geminiSchema(node any, defs map[string]any, depth int) any {
	switch value := node.(type) {
	case []any:
		out := make([]any, len(value))
		for i, item := range value {
			out[i] = geminiSchema(item, defs, depth)
		}
		return out

	case map[string]any:
		if ref, ok := value["$ref"].(string); ok {
			target := geminiResolveRef(ref, defs)
			if target == nil || depth >= geminiMaxRefDepth {
				return value
			}
			resolved, ok := geminiSchema(target, defs, depth+1).(map[string]any)
			if !ok {
				return value
			}
			for key, sibling := range value {
				if key != "$ref" && geminiSchemaFields[key] {
					resolved[key] = sibling
				}
			}
			return resolved
		}

		out := make(map[string]any, len(value))
		for key, field := range value {
			if !geminiSchemaFields[key] {
				continue
			}
			switch key {
			case "properties":
				properties, ok := field.(map[string]any)
				if !ok {
					out[key] = field
					continue
				}
				sanitized := make(map[string]any, len(properties))
				for name, property := range properties {
					sanitized[name] = geminiSchema(property, defs, depth)
				}
				out[key] = sanitized
			case "items", "anyOf":
				out[key] = geminiSchema(field, defs, depth)
			default:
				out[key] = field
			}
		}
		return out

	default:
		return node
	}
}

func geminiResolveRef(ref string, defs map[string]any) map[string]any {
	const prefix = "#/$defs/"
	if defs == nil || !strings.HasPrefix(ref, prefix) {
		return nil
	}
	target, _ := defs[strings.TrimPrefix(ref, prefix)].(map[string]any)
	return target
}

func geminiToolConfig(choice *contracts.ToolChoice) map[string]any {
	if choice == nil {
		return nil
	}
	config := map[string]any{}
	switch choice.Mode {
	case contracts.ToolChoiceNone:
		config["mode"] = "NONE"
	case contracts.ToolChoiceRequired:
		config["mode"] = "ANY"
		if choice.Name != "" {
			config["allowedFunctionNames"] = []string{choice.Name}
		}
	default:
		config["mode"] = "AUTO"
	}
	return map[string]any{"functionCallingConfig": config}
}

func geminiProviderMeta(signature string) any {
	if signature == "" {
		return nil
	}
	return map[string]any{"thoughtSignature": signature}
}

func geminiThoughtSignature(meta any) string {
	fields, ok := meta.(map[string]any)
	if !ok {
		return ""
	}
	signature, _ := fields["thoughtSignature"].(string)
	return signature
}
