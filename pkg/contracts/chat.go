package contracts

// Provider-neutral chat contract: role-based messages with tool calling.
//
// This is a *different* contract from the assistant protocol in taleTribe-agents,
// and keeping them apart is deliberate. The assistant protocol carries
// approvals, story references and editor context that a model provider must
// never see; this one carries BYOK keys and reservation IDs that a browser must
// never see. They share vocabulary ("text delta", "tool call") but not
// lifecycle, so they are defined independently and translated at the agents
// boundary rather than shared.
//
// GenerateRequest/GenerateResponse in contracts.go stay. Not for compatibility
// ceremony -- chapter generation, prose enhancement, next-line suggestions and
// the wizard all still call /v1/generate, and none of them are the assistant.
// Phases 2 and 3 migrate them.
//
// Phase 1 defines types and OpenAPI only. No handler reads these yet; the
// deterministic mock provider that first implements them is P2-T1.

// ChatRole is the author of a message. "tool" carries a tool result back into
// the next turn, which is why ToolCallID is part of the message rather than a
// separate envelope.
type ChatRole string

const (
	RoleSystem    ChatRole = "system"
	RoleUser      ChatRole = "user"
	RoleAssistant ChatRole = "assistant"
	RoleTool      ChatRole = "tool"
)

// ChatPartType tags a message part. Parts are typed rather than concatenated so
// a reload renders what the model actually produced.
type ChatPartType string

const (
	PartText     ChatPartType = "text"
	PartToolCall ChatPartType = "tool_call"
)

type ChatPart struct {
	Type ChatPartType `json:"type"`
	// Text is set when Type is "text".
	Text string `json:"text,omitempty"`
	// ToolCallID, Name and Arguments are set when Type is "tool_call".
	ToolCallID string `json:"tool_call_id,omitempty"`
	Name       string `json:"name,omitempty"`
	Arguments  any    `json:"arguments,omitempty"`
}

type ChatMessage struct {
	Role  ChatRole   `json:"role"`
	Parts []ChatPart `json:"parts"`
	// ToolCallID links a RoleTool message to the call it answers.
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// ToolSchema is a JSON Schema for one tool's arguments. The gateway does not
// interpret it; it forwards it to whichever provider is selected and normalizes
// the calls that come back.
type ToolSchema struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters"`
}

// ToolChoiceMode is the caller's policy, not a suggestion to the model.
type ToolChoiceMode string

const (
	ToolChoiceAuto     ToolChoiceMode = "auto"
	ToolChoiceNone     ToolChoiceMode = "none"
	ToolChoiceRequired ToolChoiceMode = "required"
)

type ToolChoice struct {
	Mode ToolChoiceMode `json:"mode"`
	// Name pins a specific tool. Only meaningful with ToolChoiceRequired.
	Name string `json:"name,omitempty"`
}

// ChatRequest is the versioned, provider-neutral request.
//
// MaxOutputTokens is required here, unlike the optional field on
// GenerateRequest, and that is the one place this contract is deliberately
// stricter. Reservations hold the true ceiling (promptTokens + maxOutputTokens)
// converted to credits so the platform stays solvent, and commit reconciles
// down to the provider's real reported usage. A streaming response does not
// change that arithmetic, but it does mean the ceiling has to be known *before*
// the first byte is written -- there is no later point at which a reservation
// could still be sized. Leaving it optional would mean guessing a ceiling for
// any caller that omitted it.
type ChatRequest struct {
	Version         int           `json:"version"`
	UserID          string        `json:"user_id"`
	Messages        []ChatMessage `json:"messages"`
	Tools           []ToolSchema  `json:"tools,omitempty"`
	ToolChoice      *ToolChoice   `json:"tool_choice,omitempty"`
	MaxOutputTokens int64         `json:"max_output_tokens"`
	Temperature     float64       `json:"temperature,omitempty"`
	Stream          bool          `json:"stream,omitempty"`

	// Billing plumbing, carried over from GenerateRequest unchanged.
	ReservationID    string `json:"reservation_id,omitempty"`
	IdempotencyKey   string `json:"idempotency_key,omitempty"`
	EstimatedCredits int64  `json:"estimated_credits,omitempty"`
	ForceMock        bool   `json:"force_mock,omitempty"`

	// BYOK: when set the caller's own key is used and no platform credit is
	// metered. Never logged, never echoed in a response.
	BYOKProvider string `json:"byok_provider,omitempty"`
	BYOKApiKey   string `json:"byok_api_key,omitempty"`
	BYOKModel    string `json:"byok_model,omitempty"`
}

// ChatEventType tags a normalized stream event. Every provider's native shape
// is translated into these, so agents never branches on the provider.
type ChatEventType string

const (
	ChatEventTextDelta     ChatEventType = "text_delta"
	ChatEventToolCallDelta ChatEventType = "tool_call_delta"
	ChatEventUsage         ChatEventType = "usage"
	ChatEventDone          ChatEventType = "done"
	ChatEventError         ChatEventType = "error"
)

// FinishReason is why generation stopped. "length" specifically means the
// MaxOutputTokens ceiling was hit, which is the caller's signal that the
// reservation was sized correctly and the output was truncated anyway.
type FinishReason string

const (
	FinishStop      FinishReason = "stop"
	FinishLength    FinishReason = "length"
	FinishToolCalls FinishReason = "tool_calls"
	FinishError     FinishReason = "error"
)

// ChatToolCallDelta streams one tool call's arguments as they arrive. Index
// disambiguates parallel calls within a turn, since ID may not be known on the
// first fragment from every provider.
type ChatToolCallDelta struct {
	Index          int    `json:"index"`
	ToolCallID     string `json:"tool_call_id,omitempty"`
	Name           string `json:"name,omitempty"`
	ArgumentsDelta string `json:"arguments_delta,omitempty"`
}

// ChatError is provider-neutral and safe to forward. Message is a stable
// description, never a provider body -- an upstream body can contain the
// prompt, and the prompt can contain the manuscript.
type ChatError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

type ChatEvent struct {
	Type     ChatEventType      `json:"type"`
	Provider string             `json:"provider,omitempty"`
	Model    string             `json:"model,omitempty"`
	Text     string             `json:"text,omitempty"`
	ToolCall *ChatToolCallDelta `json:"tool_call,omitempty"`
	Usage    *GenerateUsage     `json:"usage,omitempty"`
	// Credits is the reconciled cost, present on the usage event for platform
	// billing and zero for BYOK.
	Credits      int64        `json:"credits,omitempty"`
	FinishReason FinishReason `json:"finish_reason,omitempty"`
	Error        *ChatError   `json:"error,omitempty"`
}

// ChatResponse is the non-streaming form, for callers that do not want SSE.
type ChatResponse struct {
	Provider     string        `json:"provider"`
	Model        string        `json:"model"`
	Parts        []ChatPart    `json:"parts"`
	Usage        GenerateUsage `json:"usage"`
	Credits      int64         `json:"credits"`
	FinishReason FinishReason  `json:"finish_reason"`
}

// ChatContractVersion is the only accepted value of ChatRequest.Version. A
// mismatch is rejected rather than negotiated, matching the assistant
// protocol's single-version policy.
const ChatContractVersion = 1

// ErrPlatformBudgetExhausted is the marker the usage service returns when the
// platform-wide daily credit budget is spent. It travels in the response body
// so the gateway can tell it apart from a per-user 429 without a second field
// on every error shape.
const ErrPlatformBudgetExhausted = "platform_budget_exhausted"
