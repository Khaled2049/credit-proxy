# Security

This document describes the current security posture of creditProxy, known gaps, and a prioritized remediation backlog. It is updated as findings are addressed.

> **Context:** In production (GCP), all four services are `INGRESS_TRAFFIC_INTERNAL_ONLY`. The gateway only accepts calls from `novelsync-agents-run@story-6f89f.iam.gserviceaccount.com`, enforced by Cloud Run IAM. Inter-service calls (gateway → usage/llmproxy/ledger) use OIDC identity tokens. Cloud Run provides TLS termination on all services. The gaps below apply to local docker-compose (no auth, plain HTTP) and any scenario where a service is inadvertently exposed beyond its intended caller.

---

## Current Protections

| Area | What is in place |
|------|-----------------|
| Credit atomicity | Redis Lua scripts (reserve/commit/release) prevent race conditions |
| Reservation TTL | Stuck reservations expire after 180 s and are auto-cleaned by Redis |
| Ledger idempotency | Ledger enforces `ON CONFLICT (idempotency_key) DO UPDATE` to dedupe audit events |
| BYOK isolation | BYOK requests skip platform credits; usage is still audited in the ledger |
| New-user credits | `SetNX` atomically grants `INITIAL_CREDITS` only on first reservation |
| Commit reconciliation | Over/under-spend is reconciled atomically at commit time |
| Mock fallback | `LLM_PROVIDER=mock` lets local dev run without real API keys |

---

## Known Gaps

Findings are grouped by severity. Severity reflects risk if creditProxy is reachable from the internet.

### Critical

**C1 — No authentication on any endpoint**
The gateway (`POST /v1/generate`), usage service (`POST /v1/credits/purchase`, `POST /v1/reservations`), llmproxy (`POST /v1/generate`), and ledger (`POST /v1/events`, `GET /v1/users/{id}/ledger`) accept requests from any caller with no token, signature, or credential check. Any client that can reach the port can spend arbitrary users' credits, grant themselves free credits, call platform LLM keys directly, or read/write audit events.

> **Production (GCP):** Partially mitigated. Cloud Run IAM restricts the gateway to the `novelsync-agents-run` SA only. Internal services (usage, llmproxy, ledger) require an OIDC token from the `credit-proxy-run` SA. No unauthenticated caller can reach any service from the public internet. Remaining gap: the application layer does not verify the caller's identity — any service holding a valid GCP identity token for the right SA can call any endpoint.

**C2 — user_id is caller-supplied with no verification**
The gateway and usage service trust the `user_id` field in the request body. A caller can impersonate any user, charge credits to another account, or drain another user's balance.

**C3 — Internal services exposed on host ports**
`docker-compose.yml` exposes usage (`:8081`), llmproxy (`:8082`), Redis (`:6379`), and Postgres (`:5432`) on the host network. This means:
- Usage and llmproxy can be called directly, bypassing gateway credit logic entirely.
- Direct llmproxy callers can use the platform provider without credit reservation.
- Redis credit balances can be read or manipulated without auth.
- Postgres ledger can be read or modified with default credentials (`postgres:postgres`).

**C4 — No semantic upper bound on `max_output_tokens` or prompt length**
`httpx.ReadJSON()` caps raw JSON bodies at 1 MiB, but the gateway only checks that `max_output_tokens > 0` and `prompt != ""`. A caller can request `max_output_tokens: 9999999` or submit a near-1 MiB prompt, causing unbounded token reservation, LLM API quota exhaustion, provider errors, or memory pressure.

**C5 — No TLS on any service**
All inter-service and client-to-gateway communication is plain HTTP. Secrets (BYOK API keys, prompts, user IDs) travel unencrypted.

> **Production (GCP):** Resolved. Cloud Run terminates TLS on all services. All inter-service calls use `https://` Cloud Run URLs. Applies to local docker-compose only.

**C6 — Gemini API key exposed in URL query string**
`cmd/llmproxy/gemini.go` appends the Gemini key as `?key=...` in the request URL. Query strings are logged by proxies, firewalls, CDNs, and HTTP access logs, making the key easily extractable.

**C7 — Default Postgres credentials and `sslmode=disable`**
`docker-compose.yml` and `cmd/ledger/main.go` use `postgres:postgres` with `sslmode=disable`. Anyone who can reach port 5432 has full admin access to the ledger.

**C8 — Gateway idempotency does not make generation retries safe**
The ledger dedupes events by `idempotency_key`, but the gateway does not use the key to dedupe the full generation operation. Retrying `POST /v1/generate` with the same key can still reserve credits and call the LLM again; only ledger event writes are deduped.

### High

**H1 — No rate limiting on any endpoint**
No request throttling exists on gateway, usage, or llmproxy. Attackers can flood endpoints to exhaust resources, perform credit-draining attacks, or trigger API provider rate limits on platform keys.

**H2 — Ledger idempotency key is global, not scoped to user**
The ledger's `idempotency_key` uniqueness constraint is global, not per-user. A collision or guessed key can cause audit events to be no-op upserts across users, creating audit mis-attribution or missing events. It does not directly bypass gateway credit deduction, because Redis reservation happens before ledger writes.

**H3 — Provider error bodies are returned directly**
`cmd/llmproxy/main.go` returns `err.Error()` directly in HTTP error responses. Provider errors may include request metadata, prompt fragments, model names, or, for BYOK flows, API key material echoed by an upstream service.

**H4 — Zero-credit commit refunds reserved credits**
`handleCommit` allows `actual_credits == 0`. A caller can reserve 1 000 credits, then commit with `actual_credits: 0`, receiving a full refund. The minimum should be 1.

**H5 — HTTP servers have no read/header/write timeouts**
All services call `http.ListenAndServe` directly. Without `ReadHeaderTimeout`, `ReadTimeout`, and `WriteTimeout`, slowloris-style clients can hold connections open and exhaust service resources.

**H6 — No CORS policy is defined for browser exposure**
The service currently assumes trusted server-to-server callers. If the gateway is exposed directly to browsers, CORS behavior must be explicit and restrictive; otherwise operators may add permissive proxy-level CORS without understanding the billing impact.

### Medium

**M1 — `INITIAL_CREDITS` has no upper bound validation**
`strconv.ParseInt` is called without checking the result against a sane maximum. A misconfigured `INITIAL_CREDITS=9223372036854775807` (max int64) would grant every new user the maximum possible balance.

**M2 — Reservation TTL is hardcoded at 180 s**
If an LLM request takes longer than 3 minutes (plausible for long story generation), the reservation expires while the request is in flight. The commit then hits a missing-key error, and the user is neither charged nor refunded cleanly.

**M3 — Token estimation uses char/4 heuristic**
`pkg/tokens` estimates tokens as `ceil(len(text) / 4)`. This is inaccurate — especially for non-Latin scripts (Chinese, Arabic) where one character is one token, and for code where tokens are shorter. Over/under-estimates affect billing fairness.

**M4 — Request body cap is implicit and undocumented**
`httpx.ReadJSON()` uses a 1 MiB `io.LimitReader`, but the gateway OpenAPI/docs do not describe this limit. Clients receive a generic JSON decode error instead of a clear 413-style response.

**M5 — Secrets at rest are not addressed**
Redis and Postgres data volumes are unencrypted by this stack, and there is no backup/restore or retention policy for ledger data. This is acceptable for local demo use but should be documented before production deployment.

### Low

**L1 — Redis has no password in Docker Compose**
Redis is started without `requirepass`, meaning any process that can reach port 6379 has full read/write access to credit balances.

**L2 — Silent mock fallback on misconfiguration**
If `LLM_PROVIDER` is unset and `LLM_MOCK_MODE` defaults to `true`, requests succeed silently with canned responses. A configuration error masquerades as success.

---

## Next Steps — Prioritized Remediation Backlog

Work these roughly in order. Items marked with `*` are prerequisites for production exposure.

### Phase 1 — Must fix before any public exposure

1. **~~`*` Add service-to-service auth on the gateway.~~** ✅ Done (GCP)
   Cloud Run IAM restricts the gateway to `novelsync-agents-run` SA. Internal services require OIDC tokens from `credit-proxy-run` SA. `pkg/httpx.PostJSON` and `agents/storyAgent/llm_provider.py` both attach identity tokens automatically on GCP. Local docker-compose remains unauthenticated.

2. **`*` Remove host-port bindings for internal services.**
   In `docker-compose.yml`, remove `ports:` from `usage`, `llmproxy`, `redis`, and `postgres`. Only the gateway (`:8080`) should be reachable from the host. Internal services communicate over the Docker network.

3. **`*` Validate user_id against the auth token.**
   The caller's identity should be asserted, not self-reported. In the simplest form: the service calling the gateway signs requests with a service-account credential that encodes the real `user_id`; the gateway verifies the signature before accepting the claim.

4. **`*` Cap `max_output_tokens` and prompt length.**
   Add hard limits in `gateway/main.go`:
   - `max_output_tokens` ≤ 8 192 (or configurable `MAX_OUTPUT_TOKENS` env var)
   - `prompt` length ≤ 64 000 characters (or configurable `MAX_PROMPT_BYTES`)
   Return 400 if exceeded.

5. **~~`*` Add TLS termination.~~** ✅ Done (GCP)
   Cloud Run terminates TLS on all services. All Cloud Run service URLs are `https://`. Applies to local docker-compose only.

6. **`*` Move Gemini key to a request header.**
   In `cmd/llmproxy/gemini.go`, pass the key in the `X-Goog-Api-Key` header instead of the query string to keep it out of URL logs.

7. **`*` Set Redis `requirepass` and Postgres non-default credentials.**
   Wire passwords via env vars (`REDIS_PASSWORD`, `POSTGRES_PASSWORD`) in `docker-compose.yml` and the respective service configs.

### Phase 2 — Harden before multi-tenant production

8. **Make generation idempotency cover the full operation.**
   Store a request/response record keyed by `(user_id, idempotency_key)` before calling the LLM. On retry, return the previous successful response or in-flight status instead of reserving credits and calling the model again.

9. **Fix ledger idempotency key scoping.**
   Change the ledger's unique constraint from `UNIQUE(idempotency_key)` to `UNIQUE(user_id, idempotency_key)`. Update the upsert query accordingly.

10. **Add rate limiting.**
   Add a per-user token-bucket rate limiter on the gateway (e.g. `golang.org/x/time/rate`). Suggested defaults: 10 req/min per user, 100 req/min global. Expose limits as env vars.

11. **Prevent zero-credit commits.**
    In `handleCommit`, add: `if req.ActualCredits == 0 { http.Error(..., "actual_credits must be positive", 400) }`.

12. **Add `INITIAL_CREDITS` sanity cap.**
    In `cmd/usage/main.go`, validate `initialCredits ≤ MAX_INITIAL_CREDITS` (e.g. 1 000 000). Log a fatal error if the value exceeds the cap.

13. **Sanitize provider errors before returning them.**
    In `cmd/llmproxy/main.go`, map provider failures to stable error codes and redact known sensitive values (`byok_api_key`, provider keys, Authorization headers) before returning anything to the caller.

14. **Make reservation TTL configurable.**
    Accept `TTLSeconds` from the gateway request (already in the contract) and set an env-var maximum (e.g. `MAX_RESERVATION_TTL_SECONDS=600`).

15. **Add HTTP server timeouts.**
    Replace `http.ListenAndServe` with configured `http.Server` instances using `ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout`, and `IdleTimeout` for every service.

16. **Return explicit request-size errors.**
    Replace the bare `io.LimitReader` decode failure with a configured max body reader and a clear 413 response when the request body exceeds `MAX_REQUEST_BYTES`.

### Phase 3 — Longer-term improvements

17. **Replace char/4 token estimation with a real tokenizer.**
    Use `tiktoken-go` (for OpenAI-compatible models) or the Gemini token-count API to get accurate counts. This improves billing fairness and prevents commit reconciliation errors.

18. **Add structured audit logging.**
    Emit structured JSON logs (zerolog or zap) for every `handleGenerate` call including `user_id`, `provider`, `byok: bool`, `tokens_estimated`, `tokens_actual`. Exclude API keys and prompt content.

19. **Add mTLS between internal services.**
    Replace plain HTTP between gateway↔usage, gateway↔llmproxy, gateway↔ledger with mTLS using short-lived certificates. Eliminates the risk from any port accidentally exposed.

20. **Implement credit budget alerts.**
    Emit a Pub/Sub or webhook event when a user's balance drops below a configurable threshold. Helps detect abuse before a user is fully exhausted.

21. **Document secrets-at-rest and retention policy.**
    For production, define Redis/Postgres encryption expectations, backup access controls, ledger retention, and data deletion behavior.
