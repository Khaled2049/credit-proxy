package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	authn "github.com/kh1011/creditproxy/pkg/auth"
	"github.com/kh1011/creditproxy/pkg/httpx"
)

func adminTestServer(mode authn.Mode, upstream string) *server {
	return &server{
		usageURL:  upstream,
		ledgerURL: upstream,
		client:    httpx.NewHTTPClient(5 * time.Second),
		verifier:  authn.NewVerifier(authn.Config{Mode: mode}),
	}
}

func TestAdminRoutesCheckAdminBeforeReachingUpstream(t *testing.T) {
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		httpx.WriteJSON(w, http.StatusOK, map[string]any{})
	}))
	defer upstream.Close()
	s := adminTestServer(authn.ModeDevStrict, upstream.URL)

	for path, handler := range map[string]http.HandlerFunc{
		"/v1/admin/usage/daily":      s.handleAdminUsageDaily,
		"/v1/admin/platform/today":   s.handleAdminPlatformToday,
		"/v1/admin/usage/users":      s.handleAdminUsageUsers,
		"/v1/admin/usage/users/u1":   s.handleAdminUsageUser,
		"/v1/admin/users/u1/balance": s.handleAdminUserBalance,
	} {
		rr := httptest.NewRecorder()
		handler(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("status without a Firebase token = %d, want 401", rr.Code)
		}
	}
	if called {
		t.Fatal("an unauthorized admin request reached the upstream service")
	}
	rr := httptest.NewRecorder()
	s.handleAdminPlatformToday(rr, httptest.NewRequest(http.MethodPost, "/v1/admin/platform/today", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d, want 405", rr.Code)
	}
}

func TestAdminUsageDailyForwardsOnlyTheRange(t *testing.T) {
	var gotPath, gotQuery string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		if r.URL.Query().Get("from") == "bad" {
			http.Error(w, "from and to must be YYYY-MM-DD", http.StatusBadRequest)
			return
		}
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"days": []any{}})
	}))
	defer upstream.Close()
	s := adminTestServer(authn.ModeDev, upstream.URL)

	rr := httptest.NewRecorder()
	s.handleAdminUsageDaily(rr, httptest.NewRequest(http.MethodGet, "/v1/admin/usage/daily?from=2026-10-01&to=2026-10-22&user_id=someone", nil))
	if rr.Code != http.StatusOK || rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status = %d, cache = %q", rr.Code, rr.Header().Get("Cache-Control"))
	}
	if gotPath != "/v1/admin/usage/daily" || gotQuery != "from=2026-10-01&to=2026-10-22" {
		t.Errorf("forwarded %s?%s, want only from and to", gotPath, gotQuery)
	}

	rr = httptest.NewRecorder()
	s.handleAdminUsageDaily(rr, httptest.NewRequest(http.MethodGet, "/v1/admin/usage/daily?from=bad", nil))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("bad range = %d, want 400", rr.Code)
	}
}

func TestAdminProxyHidesUpstreamFailures(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "pq: relation ledger_events does not exist", http.StatusInternalServerError)
	}))
	defer upstream.Close()
	s := adminTestServer(authn.ModeDev, upstream.URL)

	rr := httptest.NewRecorder()
	s.handleAdminPlatformToday(rr, httptest.NewRequest(http.MethodGet, "/v1/admin/platform/today", nil))
	if rr.Code != http.StatusBadGateway || strings.Contains(rr.Body.String(), "pq:") {
		t.Errorf("status = %d body = %q, want 502 without the upstream error", rr.Code, rr.Body.String())
	}
}

func TestAdminUserRoutesForwardOnlySafePaths(t *testing.T) {
	var got []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.RequestURI())
		httpx.WriteJSON(w, http.StatusOK, map[string]any{})
	}))
	defer upstream.Close()
	s := adminTestServer(authn.ModeDev, upstream.URL)

	call := func(handler http.HandlerFunc, target string) int {
		rr := httptest.NewRecorder()
		handler(rr, httptest.NewRequest(http.MethodGet, target, nil))
		return rr.Code
	}
	call(s.handleAdminUsageUsers, "/v1/admin/usage/users?from=2026-10-01&sort=requests&limit=10&dailyLimit=100&debug=1")
	call(s.handleAdminUsageUser, "/v1/admin/usage/users/uid-1?to=2026-10-22&x=y")
	call(s.handleAdminUserBalance, "/v1/admin/users/uid-1/balance")
	want := []string{
		"/v1/admin/usage/users?dailyLimit=100&from=2026-10-01&limit=10&sort=requests",
		"/v1/admin/usage/users/uid-1?to=2026-10-22",
		"/v1/admin/users/uid-1/balance",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("forwarded:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	got = nil
	for _, path := range []string{
		"/v1/admin/usage/users/a/b",
		"/v1/admin/usage/users/" + strings.Repeat("x", 129),
		"/v1/admin/users/uid-1",
		"/v1/admin/users//balance",
		"/v1/admin/users/a%2Fb/balance",
	} {
		handler := s.handleAdminUsageUser
		if strings.HasPrefix(path, "/v1/admin/users/") {
			handler = s.handleAdminUserBalance
		}
		if code := call(handler, path); code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", path, code)
		}
	}
	if len(got) != 0 {
		t.Errorf("a malformed path reached upstream: %v", got)
	}
}
