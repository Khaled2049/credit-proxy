# Redis in creditProxy

## What Redis is used for

Redis is the **credit ledger** for the `usage` service (`:8081`). It stores two things:

| Key pattern | Type | Purpose |
|---|---|---|
| `user:credits:{userId}` | String (integer) | User's available credit balance |
| `reservation:{reservationId}` | Hash | In-flight reservation state |

### Credit balance (`user:credits:{userId}`)

An integer counter per user. Operations:
- **New user first call** → `SETNX user:credits:{uid} 10000` (grants `INITIAL_CREDITS`, atomic — only fires if key absent)
- **Purchase** → `INCRBY` adds purchased credits
- **Reserve** → `DECRBY` deducts estimated cost; expires after `TTLSeconds` (default 120s)
- **Commit** → reconciles actual vs estimated cost; refunds overage or deducts shortfall
- **Release** → `INCRBY` refunds the reserved amount (AI call failed / cancelled)

All reserve/commit/release ops run as **Lua scripts** via `EVAL` for atomicity — no race conditions between balance check and debit.

### Reservation hash (`reservation:{reservationId}`)

Tracks a single in-flight LLM call:

| Field | Value |
|---|---|
| `user_id` | Firebase UID |
| `amount` | credits reserved |
| `status` | `reserved` → `committed` \| `released` |

Reservations auto-expire: 120s TTL while `reserved`, extended to 86400s (24h) after commit/release for audit purposes.

---

## Connect to Redis

Redis runs on `localhost:6379` (mapped in `docker-compose.yml`).

### redis-cli (in the container)

```bash
docker compose exec redis redis-cli
```

### redis-cli (local install)

```bash
redis-cli -h localhost -p 6379
```

### Test the connection

```
127.0.0.1:6379> PING
PONG
```

---

## Inspect the data

### List all keys

```bash
KEYS *
```

Example output:
```
1) "user:credits:abc123uid"
2) "reservation:res_01j..."
```

> `KEYS *` is fine for dev. Never use it in production — use `SCAN` instead.

### Check a user's credit balance

```bash
GET user:credits:<firebase-uid>
```

```bash
# Example
GET user:credits:abc123uid
# → "9850"
```

### Inspect a reservation

```bash
HGETALL reservation:<reservation-id>
```

```bash
# Example
HGETALL reservation:res_01j9xyz
# → 1) "user_id"
#    2) "abc123uid"
#    3) "amount"
#    4) "150"
#    5) "status"
#    6) "reserved"
```

### Check TTL on a reservation

```bash
TTL reservation:<reservation-id>
# → seconds remaining, or -1 (no expiry), or -2 (key gone)
```

### Find all users with credits

```bash
KEYS user:credits:*
```

### Find all active reservations

```bash
KEYS reservation:*
```

### Monitor live commands (real-time)

```bash
MONITOR
```

Shows every command hitting Redis as it happens — useful for watching a live AI call flow.

---

## Common debug workflows

**User says they have no credits:**
```bash
GET user:credits:<uid>
# nil → key never set (user never made an AI call)
# "0" → balance exhausted
```

**Reservation stuck / never committed:**
```bash
HGETALL reservation:<res-id>
TTL reservation:<res-id>
# If status=reserved and TTL is near 0, it will auto-expire and credits stay deducted
# Manual refund:
INCRBY user:credits:<uid> <amount>
```

**Reset a user's balance for testing:**
```bash
SET user:credits:<uid> 10000
```

**Delete all data (wipe and restart):**
```bash
FLUSHALL
```
