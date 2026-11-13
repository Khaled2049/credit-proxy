package main

import (
	"context"
	"testing"
)

// Runs the ledger's own statements as the production group, so a grant that
// misses one (the upsert needs UPDATE, RETURNING needs SELECT) fails here.
func TestLedgerGroupGrants(t *testing.T) {
	db := ledgerTestDB(t)
	ctx := context.Background()

	if err := checkServingRole(ctx, db); err == nil {
		t.Error("checkServingRole accepted the test superuser")
	}
	if _, err := db.Exec(`SET ROLE creditproxy_ledger_app`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec(`RESET ROLE`) })

	allowed := map[string]string{
		"upsert": `INSERT INTO ledger_events (idempotency_key, user_id, event_type, credits, payload)
			VALUES ('k1', 'u1', 'credits_committed', 5, '{}')
			ON CONFLICT (idempotency_key) DO UPDATE SET idempotency_key = EXCLUDED.idempotency_key
			RETURNING id, created_at`,
		"read": `SELECT id, credits FROM ledger_events WHERE user_id = 'u1' ORDER BY id DESC LIMIT 100`,
	}
	for name, stmt := range allowed {
		for i := 0; i < 2; i++ { // the second upsert takes the conflict path
			if _, err := db.Exec(stmt); err != nil {
				t.Errorf("%s as creditproxy_ledger_app: %v", name, err)
			}
		}
	}
	for name, stmt := range map[string]string{
		"rewrite credits": `UPDATE ledger_events SET credits = 0`,
		"delete":          `DELETE FROM ledger_events`,
		"truncate":        `TRUNCATE ledger_events`,
		"create table":    `CREATE TABLE sneaky (id int)`,
		"create role":     `CREATE ROLE sneaky`,
	} {
		if _, err := db.Exec(stmt); err == nil {
			t.Errorf("%s as creditproxy_ledger_app succeeded; want permission denied", name)
		}
	}
	if err := checkServingRole(ctx, db); err != nil {
		t.Errorf("checkServingRole refused creditproxy_ledger_app: %v", err)
	}
}

func TestParseBool(t *testing.T) {
	for in, want := range map[string]bool{"true": true, "FALSE": false} {
		if got, err := parseBool(in); err != nil || got != want {
			t.Errorf("parseBool(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := parseBool("yes"); err == nil {
		t.Error("parseBool accepted yes")
	}
}
