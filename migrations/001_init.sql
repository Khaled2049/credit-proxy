CREATE TABLE IF NOT EXISTS ledger_events (
  id BIGSERIAL PRIMARY KEY,
  idempotency_key TEXT NOT NULL UNIQUE,
  user_id TEXT NOT NULL,
  reservation_id TEXT,
  event_type TEXT NOT NULL,
  credits BIGINT NOT NULL,
  payload JSONB NOT NULL DEFAULT '{}'::jsonb,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_ledger_events_user_created_at
  ON ledger_events(user_id, created_at DESC);
