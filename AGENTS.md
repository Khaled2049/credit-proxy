# Repository Guidelines — creditProxy

creditProxy is NovelSync's credit-metered LLM gateway. It owns credit
reservation, commit/release, BYOK routing, rate limits, and its audit ledger.

## Commands

- `docker compose up --build`: start the local gateway and dependencies.
- `go test ./...`: run Go tests.
- `gofmt -w .`: format changed Go files (avoid generated/vendor files).

## Rules

- Keep the reserve → provider call → commit/release lifecycle atomic and
  idempotent; do not bypass it for platform-funded requests.
- BYOK requests must not spend platform credits or persist user API keys.
- Treat provider responses and token usage as untrusted external data.
- Keep gateway APIs compatible with taleTribe-agents; agents access this
  service through `CREDIT_PROXY_URL`.
- Store configuration and secrets in `.env`/deployment secrets only; never
  commit credentials.

## Verification

Run `go test ./...` and check the gateway health endpoint. When changing
billing behavior, test reserve, commit, release, idempotency, and BYOK paths.
