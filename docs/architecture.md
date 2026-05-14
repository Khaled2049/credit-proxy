# Architecture

CreditProxy is a distributed system for credit-metered AI text generation. Clients interact with a single public gateway; the gateway orchestrates three internal services — usage (credit accounting), llmproxy (LLM calls), and ledger (audit trail).

## System Overview

```
                        ┌───────────────────────────────────────────────── ┐
                        │                  Docker Network                  │
                        │                                                  │
           HTTP         │  ┌───────────┐      ┌───────────┐                │
Client ───────────────► │  │  Gateway  │─────►│  Usage    │◄── Redis 7     │
        :8080           │  │  :8080    │      │  :8081    │                │
                        │  └─────┬─────┘      └───────────┘                │
                        │        │                                         │
                        │        ├────────────►┌───────────┐               │
                        │        │             │ LLM Proxy │──► Gemini/OpenAI│
                        │        │             │  :8082    │   /Anthropic  │
                        │        │             │           │   /Ollama/mock│
                        │        │             └───────────┘               │
                        │        │                                         │
                        │        └────────────►┌───────────┐               │
                        │                      │  Ledger   │◄── Postgres   │
                        │                      │  :8083    │               │
                        │                      └───────────┘               │
                        └───────────────────────────────────────────────── ┘
```

## Services

| Service    | Port | Responsibility                                               | Backing Store      |
| ---------- | ---- | ------------------------------------------------------------ | ------------------ |
| `gateway`  | 8080 | Orchestrates the full generate flow; BYOK credit bypass      | —                  |
| `usage`    | 8081 | Atomic credit reservation / commit / release                 | Redis 7            |
| `llmproxy` | 8082 | Multi-provider LLM router (Gemini, Claude, OpenAI, Ollama, mock); per-request BYOK | External LLM APIs |
| `ledger`   | 8083 | Append-only audit event log                                  | Postgres 16        |

Each service is a single Go binary compiled from `cmd/<name>/main.go`. All inter-service calls use plain HTTP JSON — no gRPC, no message queue.

## BYOK (Bring Your Own Key)

When a `GenerateRequest` carries non-empty `byok_provider` and `byok_api_key` fields:

- **Gateway**: skips the Usage service entirely (no reserve/commit/release). Emits a `byok_generate` ledger event for audit purposes.
- **LLM Proxy**: calls `newProviderFromBYOK()`, which constructs a one-off provider (Gemini, OpenAI, or Anthropic) from the request fields, bypassing the server-wide default provider.

Supported BYOK providers: `gemini`, `openai`, `anthropic` (also accepts `claude`).

## Shared Packages

| Package         | Purpose                                                      |
| --------------- | ------------------------------------------------------------ |
| `pkg/contracts` | All shared request/response structs across services          |
| `pkg/httpx`     | `ReadJSON`, `WriteJSON`, `PostJSON`, `NewHTTPClient`         |
| `pkg/ids`       | Prefixed ID generator — `ids.New("res")` → `res_<ts>_<hex>`  |
| `pkg/tokens`    | Token estimation heuristic (chars/4) — no external tokenizer |

## Distributed Systems Properties

**Atomicity:** Credit mutations (reserve, commit, release) run as Lua scripts inside Redis, ensuring they are atomic — no partial updates possible under concurrent load.

**Eventual consistency:** Redis holds operational credit state. Postgres holds the append-only event log. The two can diverge briefly (ledger writes are fire-and-forget from the gateway) but will converge on the correct history.

**Idempotency:** Every ledger write carries an `idempotency_key`. Postgres enforces uniqueness; re-submitted events are no-ops (`ON CONFLICT DO UPDATE` with a self-referential value). Gateway composes keys from the client-supplied `Idempotency-Key` header (e.g. `<key>:reserve`, `<key>:commit`).

**Fault tolerance:** Reservations have a TTL (default 180 s). If the gateway crashes after reserve but before commit/release, Redis automatically clears the reservation after TTL expiry — no manual cleanup needed.

## Storage Schemas

### Redis key space

```
user:credits:<userID>          →  integer (INCRBY / DECRBY)

reservation:<reservationID>    →  hash
  user_id   string
  amount    int64
  status    "reserved" | "committed" | "released"
```

### Postgres — `ledger_events`

```sql
CREATE TABLE ledger_events (
  id              BIGSERIAL PRIMARY KEY,
  idempotency_key TEXT NOT NULL UNIQUE,
  user_id         TEXT NOT NULL,
  reservation_id  TEXT,
  event_type      TEXT NOT NULL,   -- credits_reserved | credits_committed | credits_released | commit_failed
  credits         BIGINT NOT NULL,
  payload         JSONB NOT NULL DEFAULT '{}'::jsonb,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Fast lookup by user ordered by time (ledger history endpoint)
CREATE INDEX idx_ledger_events_user_created_at ON ledger_events(user_id, created_at DESC);
```

Schema is auto-applied at startup by the ledger service — no external migration runner required.

## Build & Deploy

All four services share a single multi-stage `Dockerfile`. The `SERVICE` build arg selects which `cmd/<name>` to compile:

```dockerfile
ARG SERVICE=gateway
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/service ./cmd/${SERVICE}
```

The final image runs as a non-root user (`appuser`, UID 10001) on Alpine 3.20.

`docker-compose.yml` wires up all services, Redis, and Postgres with sane defaults so `docker compose up --build` produces a fully working stack with no extra configuration.
