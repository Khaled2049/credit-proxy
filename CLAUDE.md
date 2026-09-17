# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Artifacts

**Never publish an Artifact without explicit permission.** This overrides the
default behaviour of publishing finished work proactively — assume the answer is
no unless asked.

Deliver documents, reports, plans and reviews as files in the repo, and say where
they landed. If a shareable link would genuinely help, ask first and wait for a
yes before publishing.

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
1. Authenticate caller (`pkg/auth` — OIDC + Firebase token; dev mode skips OIDC)
2. Estimate token cost (`pkg/tokens`) → check platform daily cap → reserve per-user credits in usage (Redis Lua scripts)
3. Emit `credits_reserved` ledger event (fire-and-forget)
4. Call llmproxy via `INTERNAL_SERVICE_TOKEN`-authenticated request → configured provider (Gemini/OpenAI/Anthropic/mock)
5. On LLM failure: release reservation + emit `credits_released`
6. On success: commit reservation with actual token count (Lua reconciles over/under-spend)
7. Emit `credits_committed` ledger event

**Request flow for `POST /v1/chat` (gateway):** the streaming, tool-calling
endpoint the assistant uses. Same trust chain and the same reserve/commit
arithmetic, but the response arrives in pieces, so settlement is a state
machine rather than one decision:

1. Validate (`version`, required `max_output_tokens`, prompt size), resolve the
   billing user, and check the chat rate bucket
   (`MAX_CHAT_REQUESTS_PER_MINUTE_PER_USER` — separate from generate's, because
   one assistant run is several chat calls)
2. Refuse platform-funded work when `PLATFORM_INFERENCE_ENABLED=false`; BYOK and
   `force_mock` still pass
3. Reserve **before writing a byte**, so a refusal is an HTTP status rather than
   an error frame after a 200
4. Relay llmproxy's SSE frames, rewriting `credits` on the `usage` event
5. Settle: reported usage commits the real cost; a failure before any output
   releases; **anything else commits the full hold** — a dropped stream, a
   client hangup or a missing usage block all leave the platform unable to know
   what it was charged (`credits_committed_unknown` in the ledger)

Settlement runs on a context detached from the request
(`context.WithoutCancel`), because the most common reason a stream ends early is
the client disconnecting — which would otherwise cancel the commit too.

`stream: false` is served by letting llmproxy aggregate the same provider stream
into one `ChatResponse`, so there is one aggregation implementation.

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

**Usage service (Redis):** Credit state lives in three Redis key types:
- `user:credits:<userID>` — integer balance per user
- `reservation:<reservationID>` — hash with `user_id`, `amount`, `status`
- `platform:daily:<YYYY-MM-DD>` — integer counter of non-BYOK requests today (auto-expires after 25 h)
- `platform:credits:<YYYY-MM-DD>` — credits held platform-wide today (auto-expires after 25 h)

All mutations (reserve/commit/release) run as Lua scripts (`reserveScript`, `commitScript`, `releaseScript`) for atomicity. Commit script reconciles estimated vs. actual spend by adjusting balance inline. A fourth script (`platformDailyScript`) atomically increments the daily counter and rejects the request if it has reached `PLATFORM_DAILY_REQUEST_LIMIT` — this check runs before any per-user credit reservation and is skipped entirely for BYOK requests.

A fifth script (`platformCreditsScript`) does the same for *credits*: the
request cap bounds how many calls are made, this bounds how large they are,
which matters once one assistant run makes several tool-calling round trips. It
charges the estimate at reservation and reconciles on commit/release, so
`commitScript` and `releaseScript` both return the reserved amount as a third
element. Keep `PLATFORM_DAILY_CREDIT_LIMIT` above the worst case the request cap
already permits or it quietly becomes the real limit on `/v1/generate` too.

**Ledger service (Postgres):** Append-only `ledger_events` table. Idempotency enforced via `ON CONFLICT (idempotency_key) DO UPDATE` (upsert is a no-op). Schema auto-applied at startup — no migration runner needed.

**Chat providers (`ChatProvider`):** `/v1/chat` needs streaming and tool calls,
which `Provider.Generate` cannot express, so backends opt in through a second
interface instead of widening the first. A provider that does not implement it
returns 501 — mock, Ollama and Gemini do; OpenAI and Anthropic do not yet. The
mock is scripted from the
contract fixtures in `pkg/contracts/testdata/chat/` (embedded via
`pkg/contracts/fixtures.go`): `__script: <fixture>` in the last user message
replays one, `__delay: <ms>` paces the frames, `__script: hang` blocks until the
caller disconnects.

**Ollama** streams newline-delimited JSON from `/api/chat` and sends a tool call
as one complete object; **Gemini** streams SSE from `streamGenerateContent` and
has no tool-call id at all, so one is minted per call and the function *name* is
recovered from the assistant turn when building `functionResponse`. Both
normalize into the same `ChatEvent` stream, so nothing downstream branches on
the provider. A model that ignores `tool_choice: required` fails with
`unsupported_model` rather than returning prose the orchestrator would try to
parse as a tool result.

llmproxy does not open its SSE response until the first event, so a provider
that fails before producing anything still returns a real HTTP status. That is
what lets the gateway tell "nothing was billable" from "the stream died
partway".

**LLM proxy:** Supports Gemini, OpenAI, Anthropic, Ollama, and mock. Server-wide provider is selected at startup via env vars (`LLM_PROVIDER`, `LLM_MOCK_MODE`, `GEMINI_API_KEY`, etc.). Per-request **BYOK** overrides the server-wide provider: if `byok_provider` + `byok_api_key` are set in the request, `newProviderFromBYOK()` instantiates a fresh provider for that request only. Requires `INTERNAL_SERVICE_TOKEN` header from gateway when the token is configured.

**Token estimation (`pkg/tokens`):** Prompt tokens estimated at ~1.3 tokens/word. Completion estimate is `min(maxCompletion, max(256, promptTokens))` — scales with prompt size rather than always reserving worst-case max. The commit step reconciles against actual token usage.

**`pkg/` layout:**
- `auth/` — `Verifier` struct: OIDC caller verification + Firebase ID token verification. Three modes: `dev` (no checks), `dev_strict` (Firebase only), `production` (OIDC + Firebase). Used by gateway to resolve `billingUserID`.
- `contracts/` — all shared request/response structs
- `httpx/` — thin helpers: `ReadJSON`, `WriteJSON`, `PostJSON`, `NewHTTPClient`, plus `PostSSE` and `NewStreamingHTTPClient` for the chat hop. The streaming client sets no overall `Timeout` (that covers the whole body and would kill a long stream); it bounds response *headers* instead.
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
| `PLATFORM_DAILY_REQUEST_LIMIT` | `1400` | Hard ceiling on non-BYOK platform requests per UTC day. Keeps total usage under the LLM provider's free-tier RPD. BYOK requests are never counted. |
| `PLATFORM_DAILY_CREDIT_LIMIT` | `150000` | Hard ceiling on non-BYOK credits held per UTC day. Sized above what the request cap already allows so it is a backstop, not the binding limit. |
| `PLATFORM_INFERENCE_ENABLED` | `true` | Kill switch. `false` refuses platform-funded inference at the gateway; BYOK and `force_mock` keep working. |
| `MAX_CHAT_REQUESTS_PER_MINUTE_PER_USER` | `60` | Per-user rate bucket for `/v1/chat`, separate from generate's. |
| `LOCAL_UNMETERED` | `false` | Skip credit metering because inference runs on local hardware; audited as `local_generate`. llmproxy refuses to start unless the provider is Ollama on a local address. Read by gateway **and** llmproxy; not exposed in Terraform. |
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
| `AUTH_MODE` | `dev` | `dev` skips all auth; `dev_strict` requires Firebase token; `production` requires OIDC + Firebase |
| `FIREBASE_PROJECT_ID` | `""` | Required in `dev_strict` / `production` for Firebase token verification |
| `GCP_AUDIENCE` | `""` | OIDC token audience (production); derived from request Host if unset |
| `GCP_ALLOWED_CALLER_SA` | `""` | Comma-separated allowed caller emails or subject IDs; empty = any valid token |
| `INTERNAL_SERVICE_TOKEN` | `""` | Shared secret gateway sends to llmproxy via `X-Internal-Token`; empty = no enforcement |
| `MAX_OUTPUT_TOKENS` | `8192` | Gateway hard cap on `max_output_tokens` per request |
| `MAX_PROMPT_CHARS` | `64000` | Gateway hard cap on prompt length in characters |
| `MAX_REQUESTS_PER_MINUTE_PER_USER` | `10` | Per-user token-bucket rate limit at the gateway |

BYOK fields (`byok_provider`, `byok_api_key`, `byok_model`) in the request body override all env-var provider config for that request only.

Copy `.env.example` to `.env` before `docker compose up`.
