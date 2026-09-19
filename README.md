# creditProxy

**v0.2.0** — Credit-metered AI gateway for TheTaleTribe. Routes LLM requests through a reserve/commit/release credit lifecycle so the platform never overspends, with full BYOK support that bypasses billing entirely.

Four Go microservices, a Redis credit ledger, and a Postgres audit trail — all running in a single `docker compose up`.

## Key features

- **Multi-provider** — Gemini, Claude, OpenAI, Ollama, and a built-in mock for testing
- **Reserve/commit/release** — credits are held before the LLM call and reconciled to actual token usage after; unused tokens are refunded automatically
- **BYOK** — requests carrying a user's own API key skip credit reservation completely and call the provider directly
- **Platform daily cap** — hard ceiling on non-BYOK requests per UTC day keeps the platform inside the LLM provider's free tier regardless of user count
- **Free credits** — new users receive a starting balance automatically on their first request
- **Append-only ledger** — every credit event is written to Postgres with idempotency keys for safe retries
- **Per-user rate limiting** — token bucket rate limiter at the gateway prevents individual abuse

## Quick start

```bash
cp .env.example .env
docker compose up --build
```

→ [Full docs](../story/wiki/creditProxy/)
