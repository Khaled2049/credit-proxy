# creditProxy API Documentation

Four Go microservices behind a single public entry point (the **gateway**). The other three services are internal — in Docker they are only reachable on the compose network, but when running locally you can hit them directly on their ports for testing.

| Service  | Port | Role | Backing store | OpenAPI spec |
|----------|------|------|---------------|--------------|
| gateway  | 8080 | Public API: auth, rate limiting, credit orchestration | — | [`openapi/gateway.yaml`](openapi/gateway.yaml) |
| usage    | 8081 | Credit balances, reservations, platform daily cap | Redis 7 | [`openapi/usage.yaml`](openapi/usage.yaml) |
| llmproxy | 8082 | Talks to the actual LLM provider (Gemini/OpenAI/Anthropic/Ollama/mock) | Provider APIs | [`openapi/llmproxy.yaml`](openapi/llmproxy.yaml) |
| ledger   | 8083 | Append-only audit log of every credit event | Postgres 16 | [`openapi/ledger.yaml`](openapi/ledger.yaml) |

Import any of the YAML files in [`docs/openapi/`](openapi/) into Postman (*Import → File*) or Insomnia (*Create → Import*). Each spec carries its own `localhost` server URL, so requests work out of the box against `make docker-up`.

---

## Gateway (`:8080`) — public entry point

The only service clients should talk to. It authenticates the caller (`pkg/auth` — OIDC + Firebase depending on `AUTH_MODE`), enforces per-user rate limits (`MAX_REQUESTS_PER_MINUTE_PER_USER`), estimates token cost, and orchestrates the reserve → generate → commit flow across the other three services.

### `POST /v1/generate`

The main endpoint. Two paths:

**Platform-credits path (default):**
1. Validate prompt (`MAX_PROMPT_CHARS`) and `max_output_tokens` (`MAX_OUTPUT_TOKENS`).
2. Estimate credits: `(prompt tokens + max_output_tokens) / TOKENS_PER_CREDIT`.
3. Reserve credits in **usage** (checks the platform daily cap first, then the user balance).
4. Call **llmproxy** with the `X-Internal-Token` shared secret.
5. On success, commit the reservation with the provider's *actual* token usage (over/under-spend is reconciled). On LLM failure, release the reservation.
6. Every step emits an event to **ledger** (`credits_reserved`, `credits_committed`, `credits_released`, `commit_failed`).

**BYOK path** (`byok_provider` + `byok_api_key` present): skips credit reservation entirely; llmproxy builds a one-off provider from the caller's key. Only a `byok_generate` audit event is written.

Request body:

```json
{
  "user_id": "user_123",
  "prompt": "Explain Redis Lua scripts",
  "max_output_tokens": 512,
  "temperature": 0.7,
  "force_mock": false,
  "idempotency_key": "optional — also read from Idempotency-Key header, else generated",
  "byok_provider": "gemini | openai | anthropic | claude (optional)",
  "byok_api_key": "sk-... (optional)",
  "byok_model": "optional model override"
}
```

Response `200`:

```json
{
  "reservation_id": "res_…",
  "idempotency_key": "idem_…",
  "estimated_credits": 8,
  "actual_credits": 3,
  "byok": false,
  "response": {
    "output": "…generated text…",
    "model": "gemini-2.5-flash-lite",
    "usage": { "prompt_tokens": 12, "completion_tokens": 230, "total_tokens": 242 }
  }
}
```

Errors: `400` (missing prompt, prompt too long, `max_output_tokens` over cap), `401`/`403` (auth), `402` (insufficient credits), `409` (commit failed), `429` (per-user rate limit or platform daily cap), `502`/`503` (upstream failures). Error bodies are generic and include a `ref req_…` correlation ID that maps to the server logs.

### `GET /v1/users/{userId}/balance`

Proxies to usage. The `userId` is cross-checked against the caller's authenticated identity.

### `POST /v1/credits/purchase`

Proxies `{ "user_id", "credits" }` to usage. Returns `503` if the usage purchase API is disabled (`ENABLE_PURCHASE_API=false`), `429` when the per-user daily purchase cap is hit.

### `GET /health`

`{ "status": "ok", "version": …, "commit": … }`

---

## Usage (`:8081`) — credit accounting (Redis)

Owns all credit state. Every mutation is a Redis Lua script, so reserve/commit/release are atomic even under concurrent requests. New users are lazily granted `INITIAL_CREDITS` (atomic `SetNX`) on first contact from any endpoint.

Redis keys:
- `user:credits:<userID>` — integer balance
- `reservation:<reservationID>` — hash: `user_id`, `amount`, `status` (`reserved` → `committed`/`released`)
- `platform:daily:<YYYY-MM-DD>` — count of non-BYOK requests today (25 h TTL)
- `user:purchases:<userID>:<YYYY-MM-DD>` — per-user purchase count today

### `POST /v1/reservations`

Places a hold on the user's credits. First increments the platform daily counter and rejects with `429` if `PLATFORM_DAILY_REQUEST_LIMIT` is reached; then decrements the balance or returns `402 insufficient credits`. Body: `{ "reservation_id"?, "user_id", "estimated_credits", "ttl_seconds"? }` (TTL defaults to 120 s; the gateway sends 180 s).

### `POST /v1/reservations/{reservationId}/commit?user_id={userId}`

Finalizes a hold with the real cost: `{ "actual_credits": 3 }`. The Lua script refunds over-reservation or charges the difference for under-reservation (`402` if the balance can't cover it). `409` if the reservation is missing/expired or already released. Committing twice is a no-op success.

### `POST /v1/reservations/{reservationId}/release?user_id={userId}`

Cancels a hold and refunds the full reserved amount. Body: `{ "reason": "llm_failure" }` (optional). `409` if already committed.

### `GET /v1/users/{userId}/balance`

Returns `{ "user_id", "available_credits", "effective_credits" }`. Materializes the initial-credit grant for brand-new users.

### `POST /v1/credits/purchase`

Gated by `ENABLE_PURCHASE_API` (returns `404` when disabled). Enforces `MAX_PURCHASES_PER_DAY_PER_USER` (`429` when exceeded), then atomically adds credits. Returns `{ "user_id", "purchased_credits", "available_credits" }`.

---

## LLM Proxy (`:8082`) — provider adapter

The only service that talks to actual LLM APIs. Provider is chosen at startup (`LLM_PROVIDER`: `gemini`, `openai`, `anthropic`, `ollama`, `mock`), overridable per-request:

- `force_mock: true` → mock provider
- `byok_provider` + `byok_api_key` → fresh provider built from the request's key (`gemini`, `openai`, `anthropic`/`claude`)

### `POST /v1/generate`

Requires `X-Internal-Token` header when `INTERNAL_SERVICE_TOKEN` is configured (`401` otherwise). Same `GenerateRequest` body as the gateway. Returns the raw `GenerateResponse` (`output`, `model`, `usage`). When the provider reports real token usage it is passed through; otherwise usage is estimated heuristically (~1.3 tokens/word). Provider failures return a generic message with an `llm_…` correlation ref; the upstream status code is classified and forwarded so callers can distinguish bad API keys from provider outages.

### `GET /health`

Includes the active provider: `{ "status": "ok", "provider": "mock", … }`

---

## Ledger (`:8083`) — audit trail (Postgres)

Append-only record of every credit event. Never affects balances — it's the reconciliation/audit source of truth. Schema is auto-applied at startup.

### `POST /v1/events`

Body: `{ "idempotency_key", "user_id", "reservation_id"?, "event_type", "credits", "payload"? }`. Idempotent: replaying the same `idempotency_key` is a no-op upsert that returns the original row. Event types emitted by the gateway: `credits_reserved`, `credits_committed`, `credits_released`, `commit_failed`, `byok_generate`.

### `GET /v1/users/{userId}/ledger`

Returns the user's 100 most recent events, newest first: `{ "events": [ … ] }`.

---

## Testing tips

- With `AUTH_MODE=dev` (default), no auth headers are needed — just pass `user_id` in bodies.
- Use `"force_mock": true` on `/v1/generate` to exercise the whole credit flow without a real provider key.
- Send an `Idempotency-Key` header to make ledger writes for a generate call deterministic.
- `make smoke` runs an end-to-end flow against a running stack.
