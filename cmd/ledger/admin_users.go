package main

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kh1011/creditproxy/pkg/httpx"
)

// UsageUser totals one user's usage over a range. HeavyDays counts days with at
// least dailyLimit platform requests, an approximation of the Functions-side
// MAX_AI_USAGE quota, which counts calls into Functions rather than model calls.
type UsageUser struct {
	UserID           string `json:"userId"`
	PlatformRequests int64  `json:"platformRequests"`
	BYOKRequests     int64  `json:"byokRequests"`
	FailedRequests   int64  `json:"failedRequests"`
	Credits          int64  `json:"credits"`
	Tokens           int64  `json:"tokens"`
	PurchasedCredits int64  `json:"purchasedCredits"`
	ActiveDays       int64  `json:"activeDays"`
	PeakDayRequests  int64  `json:"peakDayRequests"`
	HeavyDays        int64  `json:"heavyDays"`
	MostPurchasesDay int64  `json:"mostPurchasesInADay"`
}

type UsageUsersResponse struct {
	From  string      `json:"from"`
	To    string      `json:"to"`
	Users []UsageUser `json:"users"`
}

type Purchase struct {
	At      string `json:"at"`
	Credits int64  `json:"credits"`
}

type UsageUserDetail struct {
	UsageDailyResponse
	UserID    string     `json:"userId"`
	Purchases []Purchase `json:"purchases"`
}

const (
	defaultTopUsers   = 25
	maxTopUsers       = 100
	defaultDailyLimit = 100
	maxPurchaseRows   = 50
)

const usageUsersSQL = `
WITH per_day AS (
  SELECT user_id, (created_at AT TIME ZONE 'UTC')::date AS day,
    count(*) FILTER (WHERE event_type IN ('credits_committed','credits_committed_unknown','credits_released','commit_failed')) AS platform,
    count(*) FILTER (WHERE event_type IN ('byok_generate','byok_chat')) AS byok,
    count(*) FILTER (WHERE event_type IN ('credits_released','commit_failed')) AS failed,
    COALESCE(sum(credits) FILTER (WHERE event_type IN ('credits_committed','credits_committed_unknown')), 0) AS credits,
    COALESCE(sum((payload->'actual_tokens'->>'total_tokens')::numeric) FILTER (WHERE event_type IN ('credits_committed','credits_committed_unknown','byok_generate','byok_chat')), 0) AS tokens,
    COALESCE(sum(credits) FILTER (WHERE event_type = 'credits_purchased'), 0) AS purchased,
    count(*) FILTER (WHERE event_type = 'credits_purchased') AS purchases
  FROM ledger_events
  WHERE created_at >= $1 AND created_at < $2
  GROUP BY 1, 2
)
SELECT user_id, sum(platform)::bigint, sum(byok)::bigint, sum(failed)::bigint, sum(credits)::bigint,
  sum(tokens)::bigint, sum(purchased)::bigint,
  count(*) FILTER (WHERE platform + byok > 0), max(platform), count(*) FILTER (WHERE platform >= $3), max(purchases)
FROM per_day
GROUP BY user_id
HAVING sum(platform) + sum(byok) + sum(purchased) > 0
ORDER BY CASE WHEN $4 = 'requests' THEN sum(platform) + sum(byok) ELSE sum(credits) END DESC, user_id
LIMIT $5`

// positiveInt reads an optional bounded integer query parameter.
func positiveInt(raw string, fallback, max int64) (int64, bool) {
	if raw == "" {
		return fallback, true
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 1 || n > max {
		return 0, false
	}
	return n, true
}

// handleAdminUsageUsers serves GET /v1/admin/usage/users: the heaviest users in
// a range, ordered by credits (default) or requests.
func (s *server) handleAdminUsageUsers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	from, to, err := parseUsageRange(q.Get("from"), q.Get("to"), time.Now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	sort := q.Get("sort")
	if sort == "" {
		sort = "credits"
	}
	limit, okLimit := positiveInt(q.Get("limit"), defaultTopUsers, maxTopUsers)
	dailyLimit, okDaily := positiveInt(q.Get("dailyLimit"), defaultDailyLimit, 100000)
	if (sort != "credits" && sort != "requests") || !okLimit || !okDaily {
		http.Error(w, "sort must be credits or requests; limit 1-100; dailyLimit positive", http.StatusBadRequest)
		return
	}
	rows, err := s.db.QueryContext(r.Context(), usageUsersSQL, from, to.AddDate(0, 0, 1), dailyLimit, sort, limit)
	if err != nil {
		http.Error(w, "usage query failed", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := UsageUsersResponse{From: from.Format(dayLayout), To: to.Format(dayLayout), Users: []UsageUser{}}
	for rows.Next() {
		var u UsageUser
		if err := rows.Scan(&u.UserID, &u.PlatformRequests, &u.BYOKRequests, &u.FailedRequests, &u.Credits, &u.Tokens,
			&u.PurchasedCredits, &u.ActiveDays, &u.PeakDayRequests, &u.HeavyDays, &u.MostPurchasesDay); err != nil {
			http.Error(w, "usage query failed", http.StatusInternalServerError)
			return
		}
		out.Users = append(out.Users, u)
	}
	if rows.Err() != nil {
		http.Error(w, "usage query failed", http.StatusInternalServerError)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}

// handleAdminUsageUser serves GET /v1/admin/usage/users/{userId}: one user's
// daily series and their top-ups in the range.
func (s *server) handleAdminUsageUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	userID := strings.TrimPrefix(r.URL.Path, "/v1/admin/usage/users/")
	if userID == "" || len(userID) > 128 || strings.Contains(userID, "/") {
		http.NotFound(w, r)
		return
	}
	from, to, err := parseUsageRange(r.URL.Query().Get("from"), r.URL.Query().Get("to"), time.Now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	daily, err := s.usageDaily(r.Context(), from, to, userID)
	if err != nil {
		http.Error(w, "usage query failed", http.StatusInternalServerError)
		return
	}
	rows, err := s.db.QueryContext(r.Context(), `
SELECT created_at, credits FROM ledger_events
WHERE user_id = $1 AND event_type = 'credits_purchased' AND created_at >= $2 AND created_at < $3
ORDER BY created_at DESC LIMIT $4`, userID, from, to.AddDate(0, 0, 1), maxPurchaseRows)
	if err != nil {
		http.Error(w, "usage query failed", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := UsageUserDetail{UsageDailyResponse: daily, UserID: userID, Purchases: []Purchase{}}
	for rows.Next() {
		var at time.Time
		var p Purchase
		if err := rows.Scan(&at, &p.Credits); err != nil {
			http.Error(w, "usage query failed", http.StatusInternalServerError)
			return
		}
		p.At = at.UTC().Format(time.RFC3339)
		out.Purchases = append(out.Purchases, p)
	}
	if rows.Err() != nil {
		http.Error(w, "usage query failed", http.StatusInternalServerError)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}
