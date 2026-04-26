# Credit Proxy

Open-source, Dockerized distributed system in Go for credit-metered AI usage in a novel-writing app.

## Services

- `gateway` (public API): orchestrates request flow.
- `usage`: atomically reserves/commits/releases credits in Redis.
- `llmproxy`: calls Gemini (or mock mode) and returns token usage.
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

## Example Flow

1) Purchase mock credits:

```bash
curl -sS -X POST http://localhost:8081/v1/credits/purchase \
  -H 'content-type: application/json' \
  -d '{"user_id":"u1","credits":2000}'
```

2) Generate via gateway:

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

## Key Endpoints

- Gateway: `POST /v1/generate`, `GET /healthz`
- Usage: `POST /v1/credits/purchase`, `POST /v1/reservations`, `POST /v1/reservations/{id}/commit`, `POST /v1/reservations/{id}/release`, `GET /v1/users/{id}/balance`
- LLM Proxy: `POST /v1/generate`
- Ledger: `POST /v1/events`, `GET /v1/users/{id}/ledger`

## License

MIT. See `LICENSE`.
