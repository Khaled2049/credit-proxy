package contracts

type PurchaseCreditsRequest struct {
	UserID  string `json:"user_id"`
	Credits int64  `json:"credits"`
}

type BalanceResponse struct {
	UserID            string `json:"user_id"`
	AvailableCredits  int64  `json:"available_credits"`
	ReservedCredits   int64  `json:"reserved_credits,omitempty"`
	EffectiveCredits  int64  `json:"effective_credits,omitempty"`
	LastReservationID string `json:"last_reservation_id,omitempty"`
}

type ReservationRequest struct {
	ReservationID    string `json:"reservation_id,omitempty"`
	UserID           string `json:"user_id"`
	EstimatedCredits int64  `json:"estimated_credits"`
	TTLSeconds       int64  `json:"ttl_seconds"`
}

type ReservationResponse struct {
	ReservationID string `json:"reservation_id"`
	UserID        string `json:"user_id"`
	Reserved      int64  `json:"reserved"`
	Status        string `json:"status"`
}

type CommitReservationRequest struct {
	ActualCredits int64 `json:"actual_credits"`
}

type ReleaseReservationRequest struct {
	Reason string `json:"reason"`
}

type GenerateRequest struct {
	UserID           string  `json:"user_id"`
	Prompt           string  `json:"prompt"`
	MaxOutputTokens  int64   `json:"max_output_tokens,omitempty"`
	Temperature      float64 `json:"temperature,omitempty"`
	ForceMock        bool    `json:"force_mock,omitempty"`
	ReservationID    string  `json:"reservation_id,omitempty"`
	IdempotencyKey   string  `json:"idempotency_key,omitempty"`
	EstimatedCredits int64   `json:"estimated_credits,omitempty"`
	// BYOK fields — when set, the request uses the caller's own API key and
	// skips platform credit metering.
	BYOKProvider string `json:"byok_provider,omitempty"` // "gemini" | "claude" | "openai"
	BYOKApiKey   string `json:"byok_api_key,omitempty"`
	BYOKModel    string `json:"byok_model,omitempty"`
}

type GenerateUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

type GenerateResponse struct {
	Output string        `json:"output"`
	Model  string        `json:"model"`
	Usage  GenerateUsage `json:"usage"`
}

type LedgerEventRequest struct {
	IdempotencyKey string `json:"idempotency_key"`
	UserID         string `json:"user_id"`
	ReservationID  string `json:"reservation_id,omitempty"`
	EventType      string `json:"event_type"`
	Credits        int64  `json:"credits"`
	Payload        any    `json:"payload,omitempty"`
}

type LedgerEventResponse struct {
	ID             int64  `json:"id"`
	IdempotencyKey string `json:"idempotency_key"`
	UserID         string `json:"user_id"`
	ReservationID  string `json:"reservation_id,omitempty"`
	EventType      string `json:"event_type"`
	Credits        int64  `json:"credits"`
	CreatedAt      string `json:"created_at"`
}
