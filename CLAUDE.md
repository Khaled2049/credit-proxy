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
1. Estimate token cost (`pkg/tokens`) → reserve credits in usage (Redis Lua script)
2. Emit `credits_reserved` ledger event (fire-and-forget)
3. Call llmproxy → Gemini API (or mock)
4. On LLM failure: release reservation + emit `credits_released`
5. On success: commit reservation with actual token count (Lua reconciles over/under-spend)
6. Emit `credits_committed` ledger event

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

**LLM proxy:** `LLM_MOCK_MODE=true` (default) returns a canned string without calling Gemini. Set `GEMINI_API_KEY` and `LLM_MOCK_MODE=false` for real calls. Token counts use a character/4 heuristic (`pkg/tokens`), not a real tokenizer — intentional for demo determinism.

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
| `REDIS_ADDR` | `redis:6379` | |
| `POSTGRES_DSN` | `postgres://postgres:postgres@postgres:5432/creditproxy?sslmode=disable` | |
| `GEMINI_API_KEY` | `""` | Empty triggers mock mode |
| `GEMINI_MODEL` | `gemini-2.0-flash` | |
| `LLM_MOCK_MODE` | `true` | Set `false` for real Gemini calls |
| `USAGE_SERVICE_URL` | `http://usage:8081` | Gateway config |
| `LLM_PROXY_URL` | `http://llmproxy:8082` | Gateway config |
| `LEDGER_SERVICE_URL` | `http://ledger:8083` | Gateway config |

Copy `.env.example` to `.env` before `docker compose up`.
