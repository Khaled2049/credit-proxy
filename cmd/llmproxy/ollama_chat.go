package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/kh1011/creditproxy/pkg/contracts"
	"github.com/kh1011/creditproxy/pkg/ids"
)

// Ollama streams newline-delimited JSON rather than SSE, and sends a tool call
// as one complete object instead of argument fragments. Both are normalized
// here so the gateway and agents never branch on the provider.

type ollamaToolCall struct {
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

type ollamaChatFrame struct {
	Model   string `json:"model"`
	Message struct {
		Role      string           `json:"role"`
		Content   string           `json:"content"`
		ToolCalls []ollamaToolCall `json:"tool_calls"`
	} `json:"message"`
	Done            bool   `json:"done"`
	DoneReason      string `json:"done_reason"`
	PromptEvalCount int64  `json:"prompt_eval_count"`
	EvalCount       int64  `json:"eval_count"`
}

func (o *OllamaProvider) Chat(ctx context.Context, opts ChatOpts, emit func(contracts.ChatEvent) error) error {
	body := map[string]any{
		"model":    o.model,
		"messages": ollamaMessages(opts.Messages),
		"stream":   true,
		"options": map[string]any{
			"num_predict": opts.MaxOutputTokens,
			"temperature": opts.Temperature,
		},
	}
	if tools := ollamaTools(opts.Tools); len(tools) > 0 {
		body["tools"] = tools
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/api/chat", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.client.Do(req)
	if err != nil {
		return &ProviderError{Provider: "ollama", Message: fmt.Sprintf("unreachable: %v", err)}
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return &ProviderError{Provider: "ollama", StatusCode: resp.StatusCode, Message: string(msg)}
	}

	var (
		toolIndex  int
		sawTool    bool
		finish     = contracts.FinishStop
		lastFrame  ollamaChatFrame
		scanner    = bufio.NewScanner(resp.Body)
		scanBuffer = make([]byte, 0, 64*1024)
	)
	scanner.Buffer(scanBuffer, 1<<20)

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var frame ollamaChatFrame
		if err := json.Unmarshal(line, &frame); err != nil {
			return fmt.Errorf("ollama decode: %w", err)
		}
		lastFrame = frame

		if frame.Message.Content != "" {
			if err := emit(contracts.ChatEvent{
				Type:     contracts.ChatEventTextDelta,
				Provider: "ollama",
				Model:    frame.Model,
				Text:     frame.Message.Content,
			}); err != nil {
				return err
			}
		}
		for _, call := range frame.Message.ToolCalls {
			sawTool = true
			if err := emit(contracts.ChatEvent{
				Type:     contracts.ChatEventToolCallDelta,
				Provider: "ollama",
				Model:    frame.Model,
				ToolCall: &contracts.ChatToolCallDelta{
					Index:          toolIndex,
					ToolCallID:     ids.New("call"),
					Name:           call.Function.Name,
					ArgumentsDelta: string(call.Function.Arguments),
				},
			}); err != nil {
				return err
			}
			toolIndex++
		}
		if frame.Done {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}

	// A caller that asked for a tool and got prose back has a model that cannot
	// meet the contract. Say so, rather than returning an answer the
	// orchestrator will try to parse as a tool result.
	if opts.ToolChoice != nil && opts.ToolChoice.Mode == contracts.ToolChoiceRequired && !sawTool {
		return &ProviderError{
			Provider:   "ollama",
			StatusCode: http.StatusNotFound,
			Message:    "model " + o.model + " did not produce a required tool call",
		}
	}

	switch {
	case sawTool:
		finish = contracts.FinishToolCalls
	case lastFrame.DoneReason == "length":
		finish = contracts.FinishLength
	}

	if err := emit(contracts.ChatEvent{
		Type:     contracts.ChatEventUsage,
		Provider: "ollama",
		Model:    lastFrame.Model,
		Usage: &contracts.GenerateUsage{
			PromptTokens:     lastFrame.PromptEvalCount,
			CompletionTokens: lastFrame.EvalCount,
			TotalTokens:      lastFrame.PromptEvalCount + lastFrame.EvalCount,
		},
	}); err != nil {
		return err
	}
	return emit(contracts.ChatEvent{
		Type:         contracts.ChatEventDone,
		Provider:     "ollama",
		Model:        lastFrame.Model,
		FinishReason: finish,
	})
}

func ollamaMessages(messages []contracts.ChatMessage) []map[string]any {
	out := make([]map[string]any, 0, len(messages))
	for _, message := range messages {
		converted := map[string]any{"role": string(message.Role)}
		var (
			text      string
			toolCalls []map[string]any
		)
		for _, part := range message.Parts {
			switch part.Type {
			case contracts.PartText:
				text += part.Text
			case contracts.PartToolCall:
				toolCalls = append(toolCalls, map[string]any{
					"function": map[string]any{
						"name":      part.Name,
						"arguments": part.Arguments,
					},
				})
			}
		}
		converted["content"] = text
		if len(toolCalls) > 0 {
			converted["tool_calls"] = toolCalls
		}
		out = append(out, converted)
	}
	return out
}

func ollamaTools(tools []contracts.ToolSchema) []map[string]any {
	if len(tools) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        tool.Name,
				"description": tool.Description,
				"parameters":  tool.Parameters,
			},
		})
	}
	return out
}
