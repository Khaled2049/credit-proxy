package main

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kh1011/creditproxy/pkg/httpx"
	"github.com/redis/go-redis/v9"
)

// PlatformToday is today's standing against the two platform-wide caps. Both
// counters reset when the UTC day changes.
type PlatformToday struct {
	Day          string `json:"day"`
	Requests     int64  `json:"requests"`
	RequestLimit int64  `json:"requestLimit"`
	Credits      int64  `json:"credits"`
	CreditLimit  int64  `json:"creditLimit"`
	ResetsAt     string `json:"resetsAt"`
}

// handleAdminPlatformToday serves GET /v1/admin/platform/today. Only
// creditProxy's service account can reach this service; the gateway checks
// the admin claim before proxying.
func (s *server) handleAdminPlatformToday(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	now := time.Now().UTC()
	day := now.Format("2006-01-02")
	counters, err := s.rdb.MGet(r.Context(), "platform:daily:"+day, platformCreditsKey(day)).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		http.Error(w, "platform counters unavailable", http.StatusInternalServerError)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, PlatformToday{
		Day:          day,
		Requests:     counterValue(counters, 0),
		RequestLimit: s.platformDailyLimit,
		Credits:      counterValue(counters, 1),
		CreditLimit:  s.platformCreditLimit,
		ResetsAt:     now.Truncate(24*time.Hour).AddDate(0, 0, 1).Format(time.RFC3339),
	})
}

// counterValue reads one MGET slot; a key that does not exist yet means zero.
func counterValue(values []any, i int) int64 {
	if i >= len(values) {
		return 0
	}
	raw, _ := values[i].(string)
	n, _ := strconv.ParseInt(raw, 10, 64)
	return n
}

// AdminBalance is a side-effect-free balance read. The user-facing balance
// route grants INITIAL_CREDITS on first read; an admin looking must not.
type AdminBalance struct {
	UserID           string `json:"userId"`
	HasBalance       bool   `json:"hasBalance"`
	AvailableCredits int64  `json:"availableCredits"`
}

// handleAdminUserBalance serves GET /v1/admin/users/{userId}/balance.
func (s *server) handleAdminUserBalance(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/v1/admin/users/")
	userID := strings.TrimSuffix(rest, "/balance")
	if userID == "" || userID == rest || strings.Contains(userID, "/") {
		http.NotFound(w, r)
		return
	}
	balance, err := s.rdb.Get(r.Context(), userCreditsKey(userID)).Int64()
	if errors.Is(err, redis.Nil) {
		httpx.WriteJSON(w, http.StatusOK, AdminBalance{UserID: userID})
		return
	}
	if err != nil {
		http.Error(w, "balance unavailable", http.StatusInternalServerError)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, AdminBalance{UserID: userID, HasBalance: true, AvailableCredits: balance})
}
