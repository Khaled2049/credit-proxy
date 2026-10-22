package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestUsageUsersRanksAndCountsHeavyDays(t *testing.T) {
	db := ledgerTestDB(t)
	tokens := `{"actual_tokens":{"prompt_tokens":20,"completion_tokens":10,"total_tokens":30}}`
	events := []seedEvent{
		{"byok-heavy", "byok_chat", 0, tokens, "2026-10-20T01:00:00Z"},
		{"mock-only", "mock_chat", 0, tokens, "2026-10-20T01:00:00Z"},
		{"buyer", "credits_purchased", 10000, `{}`, "2026-10-21T01:00:00Z"},
		{"buyer", "credits_purchased", 10000, `{}`, "2026-10-21T02:00:00Z"},
		{"buyer", "credits_purchased", 10000, `{}`, "2026-10-21T03:00:00Z"},
	}
	// "spender": two requests a day on the 20th and 21st, so two heavy days at dailyLimit=2.
	for _, at := range []string{"2026-10-20T01:00:00Z", "2026-10-20T02:00:00Z", "2026-10-21T01:00:00Z", "2026-10-21T02:00:00Z", "2026-10-22T01:00:00Z"} {
		events = append(events, seedEvent{"spender", "credits_committed", 10, tokens, at})
	}
	for i := 0; i < 5; i++ {
		events = append(events, seedEvent{"byok-heavy", "byok_generate", 0, tokens, "2026-10-21T05:00:00Z"})
	}
	seed(t, db, events)
	s := &server{db: db}

	get := func(query string) (int, UsageUsersResponse) {
		rr := httptest.NewRecorder()
		s.handleAdminUsageUsers(rr, httptest.NewRequest(http.MethodGet, "/v1/admin/usage/users?from=2026-10-20&to=2026-10-22"+query, nil))
		var out UsageUsersResponse
		_ = json.Unmarshal(rr.Body.Bytes(), &out)
		return rr.Code, out
	}

	code, byCredits := get("&dailyLimit=2")
	if code != http.StatusOK || len(byCredits.Users) != 3 {
		t.Fatalf("status %d users %+v; want spender, buyer and byok-heavy but not mock-only", code, byCredits.Users)
	}
	spender := byCredits.Users[0]
	want := UsageUser{UserID: "spender", PlatformRequests: 5, Credits: 50, Tokens: 150, ActiveDays: 3, PeakDayRequests: 2, HeavyDays: 2}
	if spender != want {
		t.Errorf("spender = %+v\nwant      %+v", spender, want)
	}
	for _, u := range byCredits.Users {
		if u.UserID == "buyer" && (u.PurchasedCredits != 30000 || u.MostPurchasesDay != 3 || u.ActiveDays != 0) {
			t.Errorf("buyer = %+v, want 30000 credits over 3 top-ups in one day and no active days", u)
		}
	}

	_, byRequests := get("&sort=requests&limit=1")
	if len(byRequests.Users) != 1 || byRequests.Users[0].UserID != "byok-heavy" {
		t.Errorf("top by requests = %+v, want byok-heavy (6 requests, no credits) ahead of spender (5)", byRequests.Users)
	}

	for _, bad := range []string{"&sort=tokens", "&limit=0", "&limit=101", "&dailyLimit=-1"} {
		if code, _ := get(bad); code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", bad, code)
		}
	}
}

func TestUsageUserDetail(t *testing.T) {
	db := ledgerTestDB(t)
	seed(t, db, []seedEvent{
		{"u1", "credits_committed", 4, `{}`, "2026-10-21T01:00:00Z"},
		{"u2", "credits_committed", 9, `{}`, "2026-10-21T01:00:00Z"},
		{"u1", "credits_purchased", 10000, `{}`, "2026-10-21T02:00:00Z"},
		{"u1", "credits_purchased", 50000, `{}`, "2026-10-30T02:00:00Z"}, // outside the range
	})
	s := &server{db: db}

	rr := httptest.NewRecorder()
	s.handleAdminUsageUser(rr, httptest.NewRequest(http.MethodGet, "/v1/admin/usage/users/u1?from=2026-10-20&to=2026-10-22", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	var out UsageUserDetail
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.UserID != "u1" || len(out.Days) != 3 || out.Days[1].Credits != 4 || out.Days[1].PlatformRequests != 1 {
		t.Errorf("detail = %+v, want only u1's request on the 21st", out)
	}
	if len(out.Purchases) != 1 || out.Purchases[0].Credits != 10000 || out.Purchases[0].At != "2026-10-21T02:00:00Z" {
		t.Errorf("purchases = %+v, want the one top-up inside the range", out.Purchases)
	}

	for _, path := range []string{"/v1/admin/usage/users/", "/v1/admin/usage/users/a/b"} {
		rr := httptest.NewRecorder()
		s.handleAdminUsageUser(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", path, rr.Code)
		}
	}
}
