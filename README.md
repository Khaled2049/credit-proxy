# Credit Proxy

Open-source, Dockerized distributed system in Go for credit-metered AI usage in a novel-writing app. Supports Gemini, Claude, OpenAI, and Ollama — including per-request BYOK (Bring Your Own Key) mode that bypasses platform credits.

## Services

- `gateway` (public API): orchestrates request flow.
- `usage`: atomically reserves/commits/releases credits in Redis.
- `llmproxy`: multi-provider LLM router (Gemini, Claude, OpenAI, Ollama, mock).
- `ledger`: append-only transaction ledger in Postgres with idempotency keys.

## Distributed Systems Behavior

- **Concurrency control**: Redis Lua scripts ensure atomic reservation and reconciliation.
- **Eventual consistency**: Redis is operational credit state; Postgres is append-only audit history.
- **Fault tolerance**: reservation TTL prevents stuck locks; release/commit endpoints are idempotent.

## Run Locally

```bash
cp .env.example .env
docker compose up --build
```

Gateway is available at `http://localhost:8080`.

## Example Flow — Platform Credits

New users receive `INITIAL_CREDITS` (default 10 000) automatically on their first generate request — no purchase needed. To top up manually:

```bash
curl -sS -X POST http://localhost:8081/v1/credits/purchase \
  -H 'content-type: application/json' \
  -d '{"user_id":"u1","credits":2000}'
```

Generate via gateway:

```bash
curl -sS -X POST http://localhost:8080/v1/generate \
  -H 'content-type: application/json' \
  -H 'Idempotency-Key: demo-1' \
  -d '{"user_id":"u1","prompt":"Write a dramatic opening scene for a space opera.","max_output_tokens":200}'
```

3) Check balance:

```bash
curl -sS http://localhost:8081/v1/users/u1/balance
```

4) Inspect ledger:

```bash
curl -sS http://localhost:8083/v1/users/u1/ledger
```

## BYOK (Bring Your Own Key)

Pass `byok_provider`, `byok_api_key`, and optionally `byok_model` in the generate request. The gateway skips credit reservation/commit and calls the provider directly using the supplied key. Usage is still recorded in the ledger with `event_type: "byok_generate"`.

```bash
curl -sS -X POST http://localhost:8080/v1/generate \
  -H 'content-type: application/json' \
  -H 'Idempotency-Key: byok-demo-1' \
  -d '{
    "user_id": "u1",
    "prompt": "Write a dramatic opening scene for a space opera.",
    "max_output_tokens": 200,
    "byok_provider": "claude",
    "byok_api_key": "sk-ant-...",
    "byok_model": "claude-haiku-4-5-20251001"
  }'
```

Supported `byok_provider` values: `gemini`, `openai`, `anthropic` (also accepts `claude`).

## Key Endpoints

- Gateway: `POST /v1/generate`, `GET /healthz`
- Usage: `POST /v1/credits/purchase`, `POST /v1/reservations`, `POST /v1/reservations/{id}/commit`, `POST /v1/reservations/{id}/release`, `GET /v1/users/{id}/balance`
- LLM Proxy: `POST /v1/generate`
- Ledger: `POST /v1/events`, `GET /v1/users/{id}/ledger`

## License

MIT. See `LICENSE`.
