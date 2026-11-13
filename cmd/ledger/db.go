package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// schemaSQL is idempotent and runs as the database owner: at startup locally,
// and as `ledger migrate` before each production deploy. It also makes the
// permission group the production login (creditproxy_ledger) joins; Terraform
// creates that login with SQL, never the Neon console, which would put it in
// neon_superuser.
const schemaSQL = `
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
CREATE INDEX IF NOT EXISTS idx_ledger_events_user_created_at ON ledger_events(user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_ledger_events_created_at ON ledger_events(created_at DESC);

DO $$
BEGIN
  BEGIN
    CREATE ROLE creditproxy_ledger_app NOLOGIN;
  EXCEPTION WHEN duplicate_object OR unique_violation THEN NULL;
  END;
  -- Append and read only. The upsert's no-op DO UPDATE needs UPDATE on the one
  -- column it sets; nothing else may be rewritten.
  EXECUTE format('GRANT USAGE ON SCHEMA %I TO creditproxy_ledger_app', current_schema());
  GRANT SELECT, INSERT ON ledger_events TO creditproxy_ledger_app;
  GRANT UPDATE (idempotency_key) ON ledger_events TO creditproxy_ledger_app;
  EXECUTE format('GRANT USAGE ON SEQUENCE %s TO creditproxy_ledger_app',
    pg_get_serial_sequence('ledger_events', 'id'));
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'creditproxy_ledger') THEN
    GRANT creditproxy_ledger_app TO creditproxy_ledger;
  END IF;
END $$;
`

// checkServingRole refuses a login that could change the schema or that
// inherits neon_superuser, and one that cannot reach the ledger table.
func checkServingRole(ctx context.Context, db *sql.DB) error {
	var role string
	var super, createRole, bypassRLS, createInSchema, neonSuperuser bool
	err := db.QueryRowContext(ctx, `
		SELECT current_user, r.rolsuper, r.rolcreaterole, r.rolbypassrls,
		       has_schema_privilege(current_user, current_schema(), 'CREATE'),
		       COALESCE((SELECT pg_has_role(current_user, n.oid, 'MEMBER')
		                   FROM pg_roles n WHERE n.rolname = 'neon_superuser'), false)
		  FROM pg_roles r WHERE r.rolname = current_user`).
		Scan(&role, &super, &createRole, &bypassRLS, &createInSchema, &neonSuperuser)
	if err != nil {
		return fmt.Errorf("checking the database role: %w", err)
	}
	var problems []string
	for _, p := range []struct {
		bad  bool
		what string
	}{
		{super, "is a superuser"},
		{createRole, "can create roles"},
		{bypassRLS, "bypasses row-level security"},
		{createInSchema, "can create objects in the schema"},
		{neonSuperuser, "is a member of neon_superuser"},
	} {
		if p.bad {
			problems = append(problems, p.what)
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("database role %s %s; serve as creditproxy_ledger (docs/ledger-database.md)",
			role, strings.Join(problems, ", "))
	}
	if _, err := db.ExecContext(ctx, `SELECT 1 FROM ledger_events LIMIT 0`); err != nil {
		return fmt.Errorf("ledger_events is not readable; run `ledger migrate` as the owner first: %w", err)
	}
	return nil
}

func parseBool(v string) (bool, error) {
	switch strings.ToLower(v) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	return false, fmt.Errorf("want true or false, got %q", v)
}
