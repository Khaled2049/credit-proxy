package main

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/kh1011/creditproxy/pkg/contracts"
)

func schemaFromJSON(t *testing.T, raw string) any {
	t.Helper()
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return value
}

func TestGeminiToolsStripsUnsupportedSchemaFields(t *testing.T) {
	tools := []contracts.ToolSchema{{
		Name:        "search_story",
		Description: "Search the manuscript",
		Parameters: schemaFromJSON(t, `{
			"$schema": "https://json-schema.org/draft/2020-12/schema",
			"additionalProperties": false,
			"type": "object",
			"title": "SearchStoryArgs",
			"properties": {
				"query": {"type": "string", "minLength": 1, "maxLength": 500, "title": "Query"},
				"limit": {"type": "integer", "default": 8, "minimum": 1, "maximum": 20, "exclusiveMinimum": 0}
			},
			"required": ["query"]
		}`),
	}}

	declarations := geminiTools(tools)
	if len(declarations) != 1 {
		t.Fatalf("declarations = %d, want 1", len(declarations))
	}
	parameters, ok := declarations[0]["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("parameters = %T, want map", declarations[0]["parameters"])
	}
	for _, field := range []string{"additionalProperties", "$schema"} {
		if _, present := parameters[field]; present {
			t.Errorf("%s survived; Gemini rejects the whole request on it", field)
		}
	}
	properties := parameters["properties"].(map[string]any)
	limit := properties["limit"].(map[string]any)
	if _, present := limit["exclusiveMinimum"]; present {
		t.Error("exclusiveMinimum survived inside a property schema")
	}
	if limit["default"] != float64(8) || limit["maximum"] != float64(20) {
		t.Errorf("supported bounds were dropped: %+v", limit)
	}
	if !reflect.DeepEqual(parameters["required"], []any{"query"}) {
		t.Errorf("required = %+v, want [query]", parameters["required"])
	}
	if parameters["title"] != "SearchStoryArgs" {
		t.Errorf("title = %v, want it preserved", parameters["title"])
	}
}

func TestGeminiToolsInlinesRefs(t *testing.T) {
	tools := []contracts.ToolSchema{{
		Name: "propose_editor_edit",
		Parameters: schemaFromJSON(t, `{
			"type": "object",
			"$defs": {
				"Operation": {
					"type": "object",
					"additionalProperties": false,
					"properties": {"kind": {"type": "string", "enum": ["replace"]}}
				}
			},
			"properties": {
				"operation": {"$ref": "#/$defs/Operation", "description": "the edit"},
				"operations": {"type": "array", "items": {"$ref": "#/$defs/Operation"}}
			}
		}`),
	}}

	parameters := geminiTools(tools)[0]["parameters"].(map[string]any)
	if _, present := parameters["$defs"]; present {
		t.Error("$defs survived at the root")
	}
	properties := parameters["properties"].(map[string]any)

	operation := properties["operation"].(map[string]any)
	if _, present := operation["$ref"]; present {
		t.Fatal("$ref was not inlined")
	}
	if operation["type"] != "object" {
		t.Errorf("inlined schema lost its type: %+v", operation)
	}
	if _, present := operation["additionalProperties"]; present {
		t.Error("inlined schema was not itself sanitized")
	}
	if operation["description"] != "the edit" {
		t.Errorf("sibling of $ref was dropped: %+v", operation)
	}

	item := properties["operations"].(map[string]any)["items"].(map[string]any)
	if _, present := item["$ref"]; present {
		t.Error("$ref under items was not inlined")
	}
}

func TestGeminiToolsKeepsUnresolvableRef(t *testing.T) {
	tools := []contracts.ToolSchema{{
		Name: "recursive",
		Parameters: schemaFromJSON(t, `{
			"type": "object",
			"properties": {"next": {"$ref": "#/$defs/Missing"}}
		}`),
	}}

	parameters := geminiTools(tools)[0]["parameters"].(map[string]any)
	next := parameters["properties"].(map[string]any)["next"].(map[string]any)
	if next["$ref"] != "#/$defs/Missing" {
		t.Errorf("next = %+v, want the ref left in place so Gemini rejects it", next)
	}
}

func TestGeminiToolsKeepsDefaultObjectIntact(t *testing.T) {
	tools := []contracts.ToolSchema{{
		Name: "with_default",
		Parameters: schemaFromJSON(t, `{
			"type": "object",
			"properties": {
				"window": {"type": "object", "default": {"offset": 0, "limit": 8000}}
			}
		}`),
	}}

	parameters := geminiTools(tools)[0]["parameters"].(map[string]any)
	window := parameters["properties"].(map[string]any)["window"].(map[string]any)
	value, ok := window["default"].(map[string]any)
	if !ok {
		t.Fatalf("default = %T, want map", window["default"])
	}
	if value["offset"] != float64(0) || value["limit"] != float64(8000) {
		t.Errorf("default value was filtered as if it were a schema: %+v", value)
	}
}

func TestGeminiChatEmitsThoughtSignature(t *testing.T) {
	body := strings.Join([]string{
		`data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"read_current_editor","args":{}},"thoughtSignature":"sig-abc"}]}}]}`,
		`data: {"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":2,"totalTokenCount":12}}`,
	}, "\n\n")
	provider := &GeminiProvider{apiKey: "k", model: "gemini-3.5-flash", client: fixedResponseClient(200, body)}

	events := collect(t, func(emit func(contracts.ChatEvent) error) error {
		return provider.Chat(context.Background(), ChatOpts{MaxOutputTokens: 128}, emit)
	})

	var call *contracts.ChatToolCallDelta
	for _, event := range events {
		if event.Type == contracts.ChatEventToolCallDelta {
			call = event.ToolCall
		}
	}
	if call == nil {
		t.Fatal("no tool call emitted")
	}
	meta, ok := call.ProviderMeta.(map[string]any)
	if !ok {
		t.Fatalf("ProviderMeta = %T, want map", call.ProviderMeta)
	}
	if meta["thoughtSignature"] != "sig-abc" {
		t.Errorf("thoughtSignature = %v, want sig-abc", meta["thoughtSignature"])
	}
}

func TestGeminiContentsReplaysThoughtSignature(t *testing.T) {
	messages := []contracts.ChatMessage{
		{Role: contracts.RoleUser, Parts: []contracts.ChatPart{{Type: contracts.PartText, Text: "improve this"}}},
		{Role: contracts.RoleAssistant, Parts: []contracts.ChatPart{{
			Type:         contracts.PartToolCall,
			ToolCallID:   "call-1",
			Name:         "read_current_editor",
			Arguments:    map[string]any{},
			ProviderMeta: map[string]any{"thoughtSignature": "sig-abc"},
		}}},
		{Role: contracts.RoleTool, ToolCallID: "call-1", Parts: []contracts.ChatPart{{Type: contracts.PartText, Text: "the selection"}}},
	}

	contents, _ := geminiContents(messages)
	if len(contents) != 3 {
		t.Fatalf("contents = %d, want 3", len(contents))
	}
	parts := contents[1]["parts"].([]map[string]any)
	if parts[0]["thoughtSignature"] != "sig-abc" {
		t.Errorf("thoughtSignature = %v; Gemini 3.x rejects the turn without it", parts[0]["thoughtSignature"])
	}
}

func TestGeminiContentsOmitsAbsentThoughtSignature(t *testing.T) {
	messages := []contracts.ChatMessage{
		{Role: contracts.RoleAssistant, Parts: []contracts.ChatPart{{
			Type:       contracts.PartToolCall,
			ToolCallID: "call-1",
			Name:       "read_current_editor",
			Arguments:  map[string]any{},
		}}},
	}

	contents, _ := geminiContents(messages)
	parts := contents[0]["parts"].([]map[string]any)
	if _, present := parts[0]["thoughtSignature"]; present {
		t.Error("an empty signature must not be sent as a field")
	}
}
