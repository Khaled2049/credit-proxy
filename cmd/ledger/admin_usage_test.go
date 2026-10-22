package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

func TestParseUsageRange(t *testing.T) {
	now := time.Date(2026, 10, 22, 15, 4, 0, 0, time.UTC)

	from, to, err := parseUsageRange("", "", now)
	if err != nil || from.Format(dayLayout) != "2026-09-23" || to.Format(dayLayout) != "2026-10-22" {
		t.Fatalf("default range = %v..%v, %v; want the 30 days ending today", from, to, err)
	}
	if _, _, err := parseUsageRange("2026-07-25", "2026-10-22", now); err != nil {
		t.Errorf("a 90-day inclusive range must be allowed: %v", err)
	}
	for _, bad := range [][2]string{
		{"2026-07-24", "2026-10-22"}, // 91 days
		{"2026-10-23", "2026-10-22"}, // reversed
		{"22/10/2026", ""},
		{"", "2026-10-22T00:00:00Z"},
	} {
		if _, _, err := parseUsageRange(bad[0], bad[1], now); err == nil {
			t.Errorf("range %q..%q should be rejected", bad[0], bad[1])
		}
	}
}

// ledgerTestDB needs a real Postgres: set LEDGER_TEST_DSN, e.g.
// postgres://postgres:postgres@localhost:5432/creditproxy?sslmode=disable.
// Each test gets a throwaway schema, dropped afterwards.
func ledgerTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("LEDGER_TEST_DSN")
	if dsn == "" {
		t.Skip("LEDGER_TEST_DSN not set")
	}
	admin, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("ledger_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatal(err)
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	db, err := sql.Open("postgres", dsn+sep+"search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		db.Close()
		admin.Exec("DROP SCHEMA " + schema + " CASCADE")
		admin.Close()
	})
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schemaSQL); err != nil {
		t.Fatal(err)
	}
	return db
}

type seedEvent struct {
	user, kind  string
	credits     int64
	payload, at string
}

func seed(t *testing.T, db *sql.DB, events []seedEvent) {
	t.Helper()
	for i, e := range events {
		if _, err := db.Exec(`INSERT INTO ledger_events (idempotency_key,user_id,event_type,credits,payload,created_at) VALUES ($1,$2,$3,$4,$5,$6)`,
			fmt.Sprintf("k%d", i), e.user, e.kind, e.credits, e.payload, e.at); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUsageDailyAggregates(t *testing.T) {
	db := ledgerTestDB(t)

	day1 := "2026-10-20T10:00:00Z"
	day1Late := "2026-10-20T23:59:59Z"
	day3 := "2026-10-22T00:00:00Z"
	outside := "2026-10-23T00:00:00Z"
	tokens := `{"actual_tokens":{"prompt_tokens":200,"completion_tokens":50,"total_tokens":250}}`
	seed(t, db, []seedEvent{
		{"u1", "credits_reserved", 9, `{}`, day1}, // not terminal: never counted
		{"u1", "credits_committed", 3, tokens, day1},
		{"u2", "credits_committed_unknown", 9, `{}`, day1},
		{"u2", "credits_released", 9, `{}`, day1Late},
		{"u3", "commit_failed", 4, `{}`, day1},
		{"u3", "byok_chat", 0, tokens, day1},
		{"u4", "mock_chat", 0, tokens, day1}, // tests are not usage
		{"u1", "credits_purchased", 10000, `{}`, day1},
		{"u1", "credits_committed", 2, tokens, day3},
		{"u1", "credits_committed", 7, tokens, outside},
	})

	s := &server{db: db}
	rr := httptest.NewRecorder()
	s.handleAdminUsageDaily(rr, httptest.NewRequest(http.MethodGet, "/v1/admin/usage/daily?from=2026-10-20&to=2026-10-22", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	var out UsageDailyResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Days) != 3 || out.Days[1] != (UsageDay{Day: "2026-10-21"}) {
		t.Fatalf("days = %+v, want three days with an empty middle one", out.Days)
	}
	want := UsageDay{
		Day: "2026-10-20", PlatformRequests: 4, BYOKRequests: 1, FailedRequests: 2, UnknownUsageRequests: 1,
		Credits: 12, PromptTokens: 400, CompletionTokens: 100, PurchasedCredits: 10000, ActiveUsers: 3,
	}
	if out.Days[0] != want {
		t.Errorf("day 1 = %+v\nwant    %+v", out.Days[0], want)
	}
	if got := out.Days[2]; got.PlatformRequests != 1 || got.Credits != 2 {
		t.Errorf("day 3 = %+v, want one request for 2 credits (the next day is outside the range)", got)
	}

	if _, err := s.usageDaily(context.Background(), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), ""); err != nil {
		t.Errorf("an empty day must not error: %v", err)
	}
}
