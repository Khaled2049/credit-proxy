package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"

	"github.com/kh1011/creditproxy/pkg/contracts"
	"github.com/kh1011/creditproxy/pkg/httpx"
	"github.com/kh1011/creditproxy/pkg/ids"
)

func (s *server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req contracts.ChatRequest
	if err := httpx.ReadJSON(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateChatRequest(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	p := s.provider
	if req.ForceMock {
		p = s.mock
	} else if byok, err := newProviderFromBYOK(contracts.GenerateRequest{
		BYOKProvider: req.BYOKProvider,
		BYOKApiKey:   req.BYOKApiKey,
		BYOKModel:    req.BYOKModel,
	}); err != nil {
		http.Error(w, "invalid byok config: "+err.Error(), http.StatusBadRequest)
		return
	} else if byok != nil {
		p = byok
	}

	chatter, ok := p.(ChatProvider)
	if !ok {
		http.Error(w, "provider does not support chat", http.StatusNotImplemented)
		return
	}

	opts := ChatOpts{
		Messages:        req.Messages,
		Tools:           req.Tools,
		ToolChoice:      req.ToolChoice,
		MaxOutputTokens: req.MaxOutputTokens,
		Temperature:     req.Temperature,
	}
	if req.Stream {
		s.streamChat(w, r, chatter, opts)
		return
	}
	s.bufferChat(w, r, chatter, opts)
}

func validateChatRequest(req *contracts.ChatRequest) error {
	if req.Version != contracts.ChatContractVersion {
		return fmt.Errorf("version must be %d", contracts.ChatContractVersion)
	}
	if len(req.Messages) == 0 {
		return errors.New("messages is required")
	}
	// Required here, unlike GenerateRequest: a reservation holds
	// prompt + max_output and has to be sized before the first streamed byte.
	if req.MaxOutputTokens <= 0 {
		return errors.New("max_output_tokens is required and must be positive")
	}
	if req.Temperature == 0 {
		req.Temperature = 0.7
	}
	return nil
}

// streamChat writes SSE frames, but only opens the response once the first
// event arrives. A provider that fails before producing anything still gets a
// real HTTP status, which is what lets the gateway tell "nothing was billable"
// apart from "the stream died partway".
func (s *server) streamChat(w http.ResponseWriter, r *http.Request, provider ChatProvider, opts ChatOpts) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	opened := false
	emit := func(event contracts.ChatEvent) error {
		payload, err := json.Marshal(event)
		if err != nil {
			return fmt.Errorf("marshal event: %w", err)
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
	}

	err := provider.Chat(r.Context(), opts, emit)
	if err == nil {
		if !opened {
			_ = emit(contracts.ChatEvent{Type: contracts.ChatEventDone, Provider: provider.Name(), FinishReason: contracts.FinishStop})
		}
		return
	}
	if errors.Is(err, context.Canceled) || r.Context().Err() != nil {
		return
	}

	ref := ids.New("llm")
	log.Printf("provider %s chat failed ref=%s: %v", provider.Name(), ref, err)
	if !opened {
		http.Error(w, "chat failed (ref "+ref+")", chatFailureStatus(err))
		return
	}
	code, retryable := chatErrorCode(err)
	_ = emit(contracts.ChatEvent{
		Type:         contracts.ChatEventError,
		Provider:     provider.Name(),
		FinishReason: contracts.FinishError,
		Error: &contracts.ChatError{
			Code:      code,
			Message:   "chat failed (ref " + ref + ")",
			Retryable: retryable,
		},
	})
}

// bufferChat collects the same stream into one ChatResponse for callers that
// do not want SSE.
func (s *server) bufferChat(w http.ResponseWriter, r *http.Request, provider ChatProvider, opts ChatOpts) {
	var (
		text      strings.Builder
		toolCalls = map[int]*contracts.ChatPart{}
		toolArgs  = map[int]*strings.Builder{}
		resp      = contracts.ChatResponse{Provider: provider.Name()}
		failure   *contracts.ChatError
	)

	err := provider.Chat(r.Context(), opts, func(event contracts.ChatEvent) error {
		if event.Model != "" {
			resp.Model = event.Model
		}
		if event.Provider != "" {
			resp.Provider = event.Provider
		}
		switch event.Type {
		case contracts.ChatEventTextDelta:
			text.WriteString(event.Text)
		case contracts.ChatEventToolCallDelta:
			if event.ToolCall == nil {
				return nil
			}
			part, ok := toolCalls[event.ToolCall.Index]
			if !ok {
				part = &contracts.ChatPart{Type: contracts.PartToolCall}
				toolCalls[event.ToolCall.Index] = part
				toolArgs[event.ToolCall.Index] = &strings.Builder{}
			}
			if event.ToolCall.ToolCallID != "" {
				part.ToolCallID = event.ToolCall.ToolCallID
			}
			if event.ToolCall.Name != "" {
				part.Name = event.ToolCall.Name
			}
			toolArgs[event.ToolCall.Index].WriteString(event.ToolCall.ArgumentsDelta)
		case contracts.ChatEventUsage:
			if event.Usage != nil {
				resp.Usage = *event.Usage
			}
			resp.Credits = event.Credits
		case contracts.ChatEventDone:
			resp.FinishReason = event.FinishReason
		case contracts.ChatEventError:
			failure = event.Error
		}
		return nil
	})

	ref := ids.New("llm")
	if err != nil {
		log.Printf("provider %s chat failed ref=%s: %v", provider.Name(), ref, err)
		http.Error(w, "chat failed (ref "+ref+")", chatFailureStatus(err))
		return
	}
	if failure != nil {
		log.Printf("provider %s chat reported error ref=%s code=%s", provider.Name(), ref, failure.Code)
		http.Error(w, "chat failed (ref "+ref+")", http.StatusBadGateway)
		return
	}

	if text.Len() > 0 {
		resp.Parts = append(resp.Parts, contracts.ChatPart{Type: contracts.PartText, Text: text.String()})
	}
	indexes := make([]int, 0, len(toolCalls))
	for index := range toolCalls {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	for _, index := range indexes {
		part := toolCalls[index]
		if raw := toolArgs[index].String(); raw != "" {
			var args any
			if err := json.Unmarshal([]byte(raw), &args); err != nil {
				log.Printf("provider %s produced unparseable tool arguments ref=%s", provider.Name(), ref)
				http.Error(w, "chat failed (ref "+ref+")", http.StatusBadGateway)
				return
			}
			part.Arguments = args
		}
		resp.Parts = append(resp.Parts, *part)
	}
	if resp.FinishReason == "" {
		resp.FinishReason = contracts.FinishStop
	}
	httpx.WriteJSON(w, http.StatusOK, resp)
}

func chatFailureStatus(err error) int {
	var perr *ProviderError
	if errors.As(err, &perr) {
		return classifyProviderStatus(perr.StatusCode)
	}
	return http.StatusBadGateway
}

func chatErrorCode(err error) (string, bool) {
	var perr *ProviderError
	if !errors.As(err, &perr) {
		return "provider_unavailable", true
	}
	switch perr.StatusCode {
	case http.StatusTooManyRequests:
		return "rate_limited", true
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusBadRequest:
		return "provider_error", false
	case http.StatusNotFound:
		return "unsupported_model", false
	default:
		return "provider_unavailable", true
	}
}
