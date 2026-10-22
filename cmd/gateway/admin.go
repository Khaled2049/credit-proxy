package main

import (
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/kh1011/creditproxy/pkg/httpx"
	"github.com/kh1011/creditproxy/pkg/ids"
)

// The admin routes return aggregates only; the ledger and usage services hold
// the data, are invokable only by creditProxy's service account, and trust the
// gateway's admin check.
func (s *server) handleAdminUsageDaily(w http.ResponseWriter, r *http.Request) {
	s.adminProxy(w, r, withQuery(s.ledgerURL+"/v1/admin/usage/daily", r, "from", "to"))
}

func (s *server) handleAdminUsageUsers(w http.ResponseWriter, r *http.Request) {
	s.adminProxy(w, r, withQuery(s.ledgerURL+"/v1/admin/usage/users", r, "from", "to", "sort", "limit", "dailyLimit"))
}

func (s *server) handleAdminUsageUser(w http.ResponseWriter, r *http.Request) {
	userID, ok := adminUserID(strings.TrimPrefix(r.URL.Path, "/v1/admin/usage/users/"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.adminProxy(w, r, withQuery(s.ledgerURL+"/v1/admin/usage/users/"+url.PathEscape(userID), r, "from", "to"))
}

// handleAdminUserBalance serves GET /v1/admin/users/{userId}/balance: any
// user's balance, read without the first-read credit grant the user-facing
// route performs, so it sits behind the admin check.
func (s *server) handleAdminUserBalance(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/admin/users/")
	userID, ok := adminUserID(strings.TrimSuffix(rest, "/balance"))
	if !ok || !strings.HasSuffix(rest, "/balance") {
		http.NotFound(w, r)
		return
	}
	s.adminProxy(w, r, s.usageURL+"/v1/admin/users/"+url.PathEscape(userID)+"/balance")
}

// adminUserID accepts a Firebase uid: non-empty, at most 128 characters and a
// single path segment.
func adminUserID(raw string) (string, bool) {
	if raw == "" || len(raw) > 128 || strings.ContainsAny(raw, "/?#%") {
		return "", false
	}
	return raw, true
}

// withQuery forwards only the named parameters, re-encoded, so nothing else in
// the caller's query string reaches an internal service.
func withQuery(target string, r *http.Request, keys ...string) string {
	query := url.Values{}
	for _, key := range keys {
		if value := r.URL.Query().Get(key); value != "" {
			query.Set(key, value)
		}
	}
	if len(query) == 0 {
		return target
	}
	return target + "?" + query.Encode()
}

func (s *server) handleAdminPlatformToday(w http.ResponseWriter, r *http.Request) {
	s.adminProxy(w, r, s.usageURL+"/v1/admin/platform/today")
}

func (s *server) adminProxy(w http.ResponseWriter, r *http.Request, target string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	adminID, authErr := s.verifier.RequireAdmin(r.Context(), r)
	if authErr != nil {
		http.Error(w, authErr.Message, authErr.StatusCode)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	reqID := ids.New("req")
	var out map[string]any
	if err := httpx.GetJSON(r.Context(), s.client, target, &out, nil); err != nil {
		var upErr *httpx.UpstreamError
		if errors.As(err, &upErr) && upErr.StatusCode == http.StatusBadRequest {
			http.Error(w, "invalid query: from and to must be YYYY-MM-DD at most 90 days apart; sort credits or requests; limit 1-100", http.StatusBadRequest)
			return
		}
		log.Printf("admin usage lookup failed req=%s admin=%s: %v", reqID, adminID, err)
		http.Error(w, "usage summary unavailable (ref "+reqID+")", http.StatusBadGateway)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}
