# API Reference

All services speak JSON over HTTP. No authentication is implemented — this is a demo system.

---

## Gateway — `:8080`

### `POST /v1/generate`

Orchestrates the full credit-metered generation flow. Credits are reserved before the LLM call and committed (or released) after.

**Headers**

| Header            | Required | Description                                          |
|-------------------|----------|------------------------------------------------------|
| `Content-Type`    | yes      | `application/json`                                   |
| `Idempotency-Key` | no       | Client-supplied key for deduplicating ledger events. Auto-generated if omitted. |

**Request body**

```json
{
  "user_id": "u1",
  "prompt": "Write a dramatic opening scene for a space opera.",
  "max_output_tokens": 200,
  "temperature": 0.7,
  "force_mock": false
}
```

| Field               | Type    | Default | Description                                   |
|---------------------|---------|---------|-----------------------------------------------|
| `user_id`           | string  | —       | Required. User whose credits are charged.     |
| `prompt`            | string  | —       | Required. Text sent to the LLM.               |
| `max_output_tokens` | integer | 256     | Upper bound on completion length.             |
| `temperature`       | float   | 0.7     | Sampling temperature (passed to Gemini).      |
| `force_mock`        | bool    | false   | Override `LLM_MOCK_MODE`; use mock response.  |

**Response `200 OK`**

```json
{
  "reservation_id": "res_20260426T120000.000000000_a1b2c3d4e5f6g7h8",
  "idempotency_key": "demo-1",
  "estimated_credits": 75,
  "actual_credits": 62,
  "response": {
    "output": "The stars burned cold above the wreckage...",
    "model": "gemini-2.0-flash",
    "usage": {
      "prompt_tokens": 11,
      "completion_tokens": 51,
      "total_tokens": 62
    }
  }
}
```

**Error responses**

| Status | Condition                                        |
|--------|--------------------------------------------------|
| 400    | Missing `user_id` or `prompt`                    |
| 402    | Insufficient credits to reserve                  |
| 502    | LLM proxy call failed                            |
| 409    | Commit reservation failed (conflict)             |

---

### `GET /healthz`

```json
{ "status": "ok" }
```

---

## Usage Service — `:8081`

### `POST /v1/credits/purchase`

Adds credits to a user's balance. Not routed through the gateway.

**Request**

```json
{ "user_id": "u1", "credits": 2000 }
```

**Response `200 OK`**

```json
{
  "user_id": "u1",
  "purchased_credits": 2000,
  "available_credits": 2000
}
```

---

### `POST /v1/reservations`

Atomically deducts estimated credits and creates a reservation.

**Request**

```json
{
  "user_id": "u1",
  "estimated_credits": 75,
  "ttl_seconds": 180,
  "reservation_id": "res_…"
}
```

`reservation_id` is optional — auto-generated if omitted. `ttl_seconds` defaults to 120.

**Response `200 OK`**

```json
{
  "reservation_id": "res_…",
  "user_id": "u1",
  "reserved": 75,
  "status": "reserved"
}
```

| Status | Condition                  |
|--------|----------------------------|
| 402    | Insufficient credits        |

---

### `POST /v1/reservations/{id}/commit?user_id={uid}`

Commits a reservation with the actual credit count. Reconciles over/under-spend atomically.

**Request**

```json
{ "actual_credits": 62 }
```

**Response `200 OK`**

```json
{
  "reservation_id": "res_…",
  "status": "committed",
  "actual_credits": 62
}
```

| Status | Condition                                        |
|--------|--------------------------------------------------|
| 402    | Actual > reserved and insufficient balance        |
| 409    | Reservation already committed or released         |

---

### `POST /v1/reservations/{id}/release?user_id={uid}`

Releases a reservation and refunds the reserved credits.

**Request**

```json
{ "reason": "llm_failure" }
```

**Response `200 OK`**

```json
{
  "reservation_id": "res_…",
  "status": "released",
  "reason": "llm_failure"
}
```

| Status | Condition                            |
|--------|--------------------------------------|
| 409    | Reservation already committed/released|

---

### `GET /v1/users/{id}/balance`

**Response `200 OK`**

```json
{
  "user_id": "u1",
  "available_credits": 1938,
  "effective_credits": 1938
}
```

---

## LLM Proxy — `:8082`

Internal service. Called only by gateway.

### `POST /v1/generate`

**Request** — same shape as `contracts.GenerateRequest`.

**Response `200 OK`**

```json
{
  "output": "The stars burned cold...",
  "model": "gemini-2.0-flash",
  "usage": {
    "prompt_tokens": 11,
    "completion_tokens": 51,
    "total_tokens": 62
  }
}
```

Token counts are estimates (`len(text) / 4`, ceiling) unless Gemini returns real counts. When `LLM_MOCK_MODE=true` (default) or `GEMINI_API_KEY` is empty, the model field is `mock-gemini` and output is `Mock response to: <prompt>`.

---

## Ledger Service — `:8083`

### `POST /v1/events`

Appends a credit event. Idempotent — re-submitting the same `idempotency_key` is a no-op.

**Request**

```json
{
  "idempotency_key": "order-abc-123:reserve",
  "user_id": "u1",
  "reservation_id": "res_…",
  "event_type": "credits_reserved",
  "credits": 75,
  "payload": { "prompt_tokens": 11, "estimated_total_tokens": 75 }
}
```

**Event types**

| `event_type`        | Emitted when                                      |
|---------------------|---------------------------------------------------|
| `credits_reserved`  | Reservation created                               |
| `credits_committed` | Reservation committed with actual spend           |
| `credits_released`  | Reservation released (e.g. LLM failure)           |
| `commit_failed`     | Commit endpoint returned an error                 |

**Response `200 OK`**

```json
{
  "id": 42,
  "idempotency_key": "order-abc-123:reserve",
  "user_id": "u1",
  "reservation_id": "res_…",
  "event_type": "credits_reserved",
  "credits": 75,
  "created_at": "2026-04-26T12:00:00.000000000Z"
}
```

---

### `GET /v1/users/{id}/ledger`

Returns the last 100 events for a user, newest first.

**Response `200 OK`**

```json
{
  "events": [
    {
      "id": 43,
      "idempotency_key": "order-abc-123:commit",
      "user_id": "u1",
      "reservation_id": "res_…",
      "event_type": "credits_committed",
      "credits": 62,
      "created_at": "2026-04-26T12:00:01.000000000Z"
    },
    {
      "id": 42,
      "idempotency_key": "order-abc-123:reserve",
      "user_id": "u1",
      "reservation_id": "res_…",
      "event_type": "credits_reserved",
      "credits": 75,
      "created_at": "2026-04-26T12:00:00.000000000Z"
    }
  ]
}
```
