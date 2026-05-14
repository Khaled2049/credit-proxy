# Local Development

## Prerequisites

- Go 1.22+
- Docker + Docker Compose (for full stack)

## Quickstart (Docker)

```bash
cp .env.example .env
# optionally set GEMINI_API_KEY and LLM_MOCK_MODE=false in .env
docker compose up --build
```

All services start automatically. Gateway is at `http://localhost:8080`.

```bash
# tail logs
make docker-logs

# stop everything
make docker-down
```

## Running Services Individually

Requires Redis and/or Postgres running locally. Override addresses via env vars.

```bash
# usage service (needs Redis)
REDIS_URL=redis://localhost:6379 make run-usage

# ledger service (needs Postgres)
POSTGRES_DSN=postgres://postgres:postgres@localhost:5432/creditproxy?sslmode=disable make run-ledger

# llmproxy (no external deps in mock mode)
make run-llmproxy

# gateway (needs all three above running)
USAGE_SERVICE_URL=http://localhost:8081 \
LLM_PROXY_URL=http://localhost:8082 \
LEDGER_SERVICE_URL=http://localhost:8083 \
make run-gateway
```

## Tests

```bash
# all packages
make test

# single package
go test ./pkg/tokens/...
go test ./cmd/usage/...
go test ./cmd/ledger/...
```

Tests are unit-only — no Redis or Postgres required. The usage service tests validate Lua script content; the token tests validate the estimation heuristic.

## Smoke Test

With the full Docker stack running:

```bash
make smoke
```

This runs four curl calls in sequence:
1. Purchase 2000 credits for user `u1` (optional — new users get `INITIAL_CREDITS` free on first request)
2. Generate text (costs ~credits based on prompt + `max_output_tokens`)
3. Check balance
4. Inspect ledger history

## Using Real Gemini

1. Get an API key from Google AI Studio
2. Set in `.env`:
   ```
   LLM_PROVIDER=gemini
   GEMINI_API_KEY=your-key-here
   GEMINI_MODEL=gemini-2.0-flash
   ```
3. `docker compose up --build`

Without `GEMINI_API_KEY`, llmproxy will error at startup when `LLM_PROVIDER=gemini`. Use `LLM_PROVIDER=mock` for a keyless local setup.

## Environment Variables

| Variable              | Default (local)                                              | Service   |
|-----------------------|--------------------------------------------------------------|-----------|
| `GATEWAY_ADDR`        | `:8080`                                                      | gateway   |
| `USAGE_SERVICE_URL`   | `http://usage:8081`                                          | gateway   |
| `LLM_PROXY_URL`       | `http://llmproxy:8082`                                       | gateway   |
| `LEDGER_SERVICE_URL`  | `http://ledger:8083`                                         | gateway   |
| `USAGE_ADDR`          | `:8081`                                                      | usage     |
| `REDIS_URL`           | `redis://redis:6379`                                         | usage     |
| `LLMPROXY_ADDR`       | `:8082`                                                      | llmproxy  |
| `LLM_PROVIDER`        | `mock`                                                       | llmproxy  |
| `GEMINI_API_KEY`      | `""`                                                         | llmproxy  |
| `GEMINI_MODEL`        | `gemini-2.0-flash`                                           | llmproxy  |
| `OPENAI_API_KEY`      | `""`                                                         | llmproxy  |
| `OPENAI_MODEL`        | `gpt-4o-mini`                                                | llmproxy  |
| `ANTHROPIC_API_KEY`   | `""`                                                         | llmproxy  |
| `ANTHROPIC_MODEL`     | `claude-sonnet-4-6`                                          | llmproxy  |
| `OLLAMA_BASE_URL`     | `http://localhost:11434`                                     | llmproxy  |
| `OLLAMA_MODEL`        | `llama3`                                                     | llmproxy  |
| `INITIAL_CREDITS`     | `10000`                                                      | usage     |
| `LEDGER_ADDR`         | `:8083`                                                      | ledger    |
| `POSTGRES_DSN`        | `postgres://postgres:postgres@postgres:5432/creditproxy?sslmode=disable` | ledger |

Docker Compose injects all of these automatically. When running services locally, Docker-internal hostnames (`redis`, `postgres`, `usage`, etc.) must be replaced with `localhost`.

## Code Formatting

```bash
make fmt
# equivalent to:
gofmt -w $(go list -f '{{.Dir}}' ./...)
```
