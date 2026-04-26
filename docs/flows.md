# Request Flows

## Generate Flow (`POST /v1/generate`)

The full happy-path sequence across all four services:

```
Client          Gateway         Usage           LLM Proxy       Ledger          Redis       Postgres
  │                │               │                │               │              │             │
  │─POST /generate►│               │                │               │              │             │
  │                │               │                │               │              │             │
  │                │──POST /v1/reservations─────────►               │              │             │
  │                │  {user_id, estimated_credits,  │               │              │             │
  │                │   reservation_id, ttl_seconds} │               │              │             │
  │                │               │                │               │              │             │
  │                │               │──Lua reserveScript────────────────────────────►             │
  │                │               │  DECRBY user:credits:<id>      │              │             │
  │                │               │  HSET reservation:<res_id>     │              │             │
  │                │               │  EXPIRE reservation:<res_id>   │              │             │
  │                │               │◄──────────────────────────────────────────────│             │
  │                │◄──200 {reservation_id}─────────│               │              │             │
  │                │               │                │               │              │             │
  │                │──POST /v1/events (fire & forget)───────────────────────────────────────────►│
  │                │  event_type: "credits_reserved" │               │             │             │
  │                │               │                │               │              │             │
  │                │──POST /v1/generate─────────────────────────────►              │             │
  │                │  {prompt, max_output_tokens, …}│               │              │             │
  │                │               │                │               │              │             │
  │                │               │                │──Gemini API──►│ (or mock)    │             │
  │                │               │                │◄──────────────│              │             │
  │                │◄──200 {output, usage}───────────────────────────              │             │
  │                │               │                │               │              │             │
  │                │──POST /v1/reservations/<id>/commit─────────────►              │             │
  │                │  ?user_id=…  {actual_credits}  │               │              │             │
  │                │               │                │               │              │             │
  │                │               │──Lua commitScript─────────────────────────────►             │
  │                │               │  reconcile reserved vs actual  │              │             │
  │                │               │  HSET status=committed         │              │             │
  │                │               │◄──────────────────────────────────────────────│             │
  │                │◄──200 {committed}──────────────│               │              │             │
  │                │               │                │               │              │             │
  │                │──POST /v1/events (fire & forget)───────────────────────────────────────────►│
  │                │  event_type: "credits_committed"│              │              │             │
  │                │               │                │               │              │             │
  │◄──200 ─────────│               │                │               │              │             │
  │  {reservation_id, estimated_credits,            │               │              │             │
  │   actual_credits, response}    │                │               │              │             │
```

## LLM Failure Path

When the LLM proxy returns an error, the gateway rolls back the reservation:

```
Gateway         Usage           LLM Proxy       Ledger
  │               │                │               │
  │──POST /v1/reservations─────────►               │
  │◄──200 reserved─────────────────│               │
  │               │                │               │
  │──POST /v1/events───────────────────────────────► credits_reserved
  │               │                │               │
  │──POST /v1/generate─────────────────────────────►
  │◄──5xx error────────────────────────────────────│
  │               │                │               │
  │──POST /v1/reservations/<id>/release────────────►
  │  reason: "llm_failure"         │               │
  │               │──Lua releaseScript─►           │
  │               │  INCRBY user:credits (refund)  │
  │               │  HSET status=released          │
  │◄──200 released─────────────────│               │
  │               │                │               │
  │──POST /v1/events───────────────────────────────► credits_released
  │               │                │               │
  │──502 Bad Gateway to client     │               │
```

## Credit Reconciliation (Commit)

The commit Lua script handles three cases atomically:

```
Case 1: actual == reserved
  ┌─────────────────────────────────────────┐
  │ No balance adjustment needed            │
  │ HSET reservation status=committed       │
  └─────────────────────────────────────────┘

Case 2: actual < reserved  (refund)
  ┌─────────────────────────────────────────┐
  │ refund = reserved - actual              │
  │ INCRBY user:credits:<id> refund         │
  │ HSET reservation status=committed       │
  │ HSET reservation amount=actual          │
  └─────────────────────────────────────────┘

Case 3: actual > reserved  (extra charge)
  ┌─────────────────────────────────────────┐
  │ extra = actual - reserved               │
  │ if balance < extra → 402 (abort)        │
  │ DECRBY user:credits:<id> extra          │
  │ HSET reservation status=committed       │
  │ HSET reservation amount=actual          │
  └─────────────────────────────────────────┘
```

## Reservation State Machine

```
                     ┌─────────────────┐
                     │                 │
         ┌───────────│   [not exists]  │
         │           │                 │
         │           └─────────────────┘
         │ POST /v1/reservations
         ▼
  ┌─────────────┐
  │             │──── TTL expires ────► key deleted (auto-cleanup)
  │  reserved   │
  │             │
  └──────┬──────┘
         │
    ┌────┴────┐
    │         │
    ▼         ▼
┌─────────┐  ┌─────────┐
│committed│  │released │
│(24h TTL)│  │(24h TTL)│
└─────────┘  └─────────┘
```

Transitions are enforced by the Lua scripts — committed/released reservations reject further state changes.

## Purchase Flow (`POST /v1/credits/purchase`)

Direct call to usage service, not routed through gateway:

```
Client          Usage           Redis
  │               │               │
  │─POST /v1/credits/purchase────►│
  │  {user_id, credits}          │               │
  │               │──INCRBY user:credits:<id> ───►
  │               │◄──────────────────────────────
  │◄──200 {available_credits}─────│
```

## Idempotency Key Composition

Gateway derives ledger idempotency keys from the client-supplied `Idempotency-Key` header:

```
Client header:  Idempotency-Key: order-abc-123

Gateway emits:
  order-abc-123:reserve   → ledger event (credits_reserved)
  order-abc-123:commit    → ledger event (credits_committed)
  order-abc-123:release   → ledger event (credits_released)   [on failure]
  order-abc-123:commit_failed → ledger event (commit_failed)  [on commit error]
```

If no header is supplied, gateway auto-generates one via `ids.New("idem")`.
