# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
# Run all tests
go test ./...

# Run tests for a single package
go test ./cmd/usage/...
go test ./pkg/tokens/...

# Format code
make fmt

# Run a single service locally (requires Redis/Postgres running)
make run-gateway
make run-usage
make run-llmproxy
make run-ledger

# Full stack via Docker
make docker-up
make docker-down
make docker-logs

# End-to-end smoke test (requires running stack)
make smoke
```

## Architecture

Four Go microservices, each in `cmd/<name>/main.go` (single-file per service). Shared types in `pkg/contracts/contracts.go`. No frameworks — stdlib `net/http` throughout.

**Request flow for `POST /v1/generate` (gateway):**

*Platform-credits path (default):*
1. Estimate token cost (`pkg/tokens`) → reserve credits in usage (Redis Lua script)
2. Emit `credits_reserved` ledger event (fire-and-forget)
3. Call llmproxy → configured provider (Gemini/OpenAI/Anthropic/mock)
4. On LLM failure: release reservation + emit `credits_released`
5. On success: commit reservation with actual token count (Lua reconciles over/under-spend)
6. Emit `credits_committed` ledger event

*BYOK path (`byok_provider` + `byok_api_key` present):*
1. Skip credit reservation entirely
2. Call llmproxy with BYOK fields; llmproxy instantiates the provider from the request
3. Emit `byok_generate` ledger event (audit only — no credit impact)

**Service ports:**
| Service | Port | Backing store |
|---------|------|---------------|
| gateway | 8080 | — |
| usage   | 8081 | Redis 7 |
| llmproxy| 8082 | Gemini API |
| ledger  | 8083 | Postgres 16 |

**Usage service (Redis):** Credit state lives in two Redis key types:
- `user:credits:<userID>` — integer balance
- `reservation:<reservationID>` — hash with `user_id`, `amount`, `status`

All mutations (reserve/commit/release) run as Lua scripts (`reserveScript`, `commitScript`, `releaseScript`) for atomicity. Commit script reconciles estimated vs. actual spend by adjusting balance inline.

**Ledger service (Postgres):** Append-only `ledger_events` table. Idempotency enforced via `ON CONFLICT (idempotency_key) DO UPDATE` (upsert is a no-op). Schema auto-applied at startup — no migration runner needed.

**LLM proxy:** Supports Gemini, OpenAI, Anthropic, Ollama, and mock. Server-wide provider is selected at startup via env vars (`LLM_PROVIDER`, `LLM_MOCK_MODE`, `GEMINI_API_KEY`, etc.). Per-request **BYOK** overrides the server-wide provider: if `byok_provider` + `byok_api_key` are set in the request, `newProviderFromBYOK()` instantiates a fresh provider for that request only. Token counts use a character/4 heuristic (`pkg/tokens`), not a real tokenizer — intentional for demo determinism.

**`pkg/` layout:**
- `contracts/` — all shared request/response structs
- `httpx/` — thin helpers: `ReadJSON`, `WriteJSON`, `PostJSON`, `NewHTTPClient`
- `ids/` — prefixed ID generator (e.g. `ids.New("res")` → `res_<uuid>`)
- `tokens/` — token estimation heuristics

## Environment Variables

| Variable | Default (Docker) | Notes |
|----------|-----------------|-------|
| `GATEWAY_ADDR` | `:8080` | |
| `USAGE_ADDR` | `:8081` | |
| `LLMPROXY_ADDR` | `:8082` | |
| `LEDGER_ADDR` | `:8083` | |
| `REDIS_URL` | `redis://redis:6379` | |
| `POSTGRES_DSN` | `postgres://postgres:postgres@postgres:5432/creditproxy?sslmode=disable` | |
| `LLM_PROVIDER` | `mock` | Provider: `gemini`, `openai`, `anthropic`, `ollama`, `mock`. Set in `.env`, wired to llmproxy via docker-compose. |
| `LLM_MOCK_MODE` | `true` | Legacy fallback: if `LLM_PROVIDER` unset and this is `true`, use mock |
| `INITIAL_CREDITS` | `10000` | Free tokens granted to new users on their first reservation (atomic `SetNX`) |
| `GEMINI_API_KEY` | `""` | Required when `LLM_PROVIDER=gemini` |
| `GEMINI_MODEL` | `gemini-2.0-flash` | |
| `OPENAI_API_KEY` | `""` | Required when `LLM_PROVIDER=openai` |
| `OPENAI_MODEL` | `gpt-4o-mini` | |
| `OPENAI_BASE_URL` | `https://api.openai.com/v1` | Override for compatible APIs |
| `ANTHROPIC_API_KEY` | `""` | Required when `LLM_PROVIDER=anthropic` |
| `ANTHROPIC_MODEL` | `claude-sonnet-4-6` | |
| `ANTHROPIC_BASE_URL` | `https://api.anthropic.com/v1` | |
| `OLLAMA_BASE_URL` | `http://localhost:11434` | |
| `OLLAMA_MODEL` | `llama3` | |
| `USAGE_SERVICE_URL` | `http://usage:8081` | Gateway config |
| `LLM_PROXY_URL` | `http://llmproxy:8082` | Gateway config |
| `LEDGER_SERVICE_URL` | `http://ledger:8083` | Gateway config |

BYOK fields (`byok_provider`, `byok_api_key`, `byok_model`) in the request body override all env-var provider config for that request only.

Copy `.env.example` to `.env` before `docker compose up`.
