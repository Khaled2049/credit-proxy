package main

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/kh1011/creditproxy/pkg/httpx"
)

const (
	dayLayout        = "2006-01-02"
	defaultUsageSpan = 30
	maxUsageSpan     = 90
)

// UsageDay aggregates one UTC day. A request is counted by its terminal event,
// so each request appears once. mock_chat and local_generate are left out:
// they are tests and local models, not usage anyone pays for.
type UsageDay struct {
	Day                  string `json:"day"`
	PlatformRequests     int64  `json:"platformRequests"`
	BYOKRequests         int64  `json:"byokRequests"`
	FailedRequests       int64  `json:"failedRequests"`
	UnknownUsageRequests int64  `json:"unknownUsageRequests"`
	Credits              int64  `json:"credits"`
	PromptTokens         int64  `json:"promptTokens"`
	CompletionTokens     int64  `json:"completionTokens"`
	PurchasedCredits     int64  `json:"purchasedCredits"`
	ActiveUsers          int64  `json:"activeUsers"`
}

type UsageDailyResponse struct {
	From string     `json:"from"`
	To   string     `json:"to"`
	Days []UsageDay `json:"days"`
}

var errUsageRange = errors.New("from and to must be YYYY-MM-DD, from <= to, at most 90 days apart")

// parseUsageRange reads an inclusive UTC day range, defaulting to the last 30
// days. The span is bounded so a request cannot scan the whole table.
func parseUsageRange(fromRaw, toRaw string, now time.Time) (time.Time, time.Time, error) {
	to := now.UTC().Truncate(24 * time.Hour)
	if toRaw != "" {
		parsed, err := time.Parse(dayLayout, toRaw)
		if err != nil {
			return time.Time{}, time.Time{}, errUsageRange
		}
		to = parsed
	}
	from := to.AddDate(0, 0, -(defaultUsageSpan - 1))
	if fromRaw != "" {
		parsed, err := time.Parse(dayLayout, fromRaw)
		if err != nil {
			return time.Time{}, time.Time{}, errUsageRange
		}
		from = parsed
	}
	if from.After(to) || to.Sub(from) >= maxUsageSpan*24*time.Hour {
		return time.Time{}, time.Time{}, errUsageRange
	}
	return from, to, nil
}

const usageDailySQL = `
SELECT to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD') AS day,
  count(*) FILTER (WHERE event_type IN ('credits_committed','credits_committed_unknown','credits_released','commit_failed')),
  count(*) FILTER (WHERE event_type IN ('byok_generate','byok_chat')),
  count(*) FILTER (WHERE event_type IN ('credits_released','commit_failed')),
  count(*) FILTER (WHERE event_type = 'credits_committed_unknown'),
  COALESCE(sum(credits) FILTER (WHERE event_type IN ('credits_committed','credits_committed_unknown')), 0),
  COALESCE(sum((payload->'actual_tokens'->>'prompt_tokens')::numeric) FILTER (WHERE event_type IN ('credits_committed','credits_committed_unknown','byok_generate','byok_chat')), 0)::bigint,
  COALESCE(sum((payload->'actual_tokens'->>'completion_tokens')::numeric) FILTER (WHERE event_type IN ('credits_committed','credits_committed_unknown','byok_generate','byok_chat')), 0)::bigint,
  COALESCE(sum(credits) FILTER (WHERE event_type = 'credits_purchased'), 0),
  count(DISTINCT user_id) FILTER (WHERE event_type IN ('credits_committed','credits_committed_unknown','credits_released','commit_failed','byok_generate','byok_chat'))
FROM ledger_events
WHERE created_at >= $1 AND created_at < $2 AND ($3 = '' OR user_id = $3)
GROUP BY 1`

// usageDaily aggregates the whole platform, or one user when userID is set.
func (s *server) usageDaily(ctx context.Context, from, to time.Time, userID string) (UsageDailyResponse, error) {
	rows, err := s.db.QueryContext(ctx, usageDailySQL, from, to.AddDate(0, 0, 1), userID)
	if err != nil {
		return UsageDailyResponse{}, err
	}
	defer rows.Close()
	byDay := map[string]UsageDay{}
	for rows.Next() {
		var d UsageDay
		if err := rows.Scan(&d.Day, &d.PlatformRequests, &d.BYOKRequests, &d.FailedRequests, &d.UnknownUsageRequests,
			&d.Credits, &d.PromptTokens, &d.CompletionTokens, &d.PurchasedCredits, &d.ActiveUsers); err != nil {
			return UsageDailyResponse{}, err
		}
		byDay[d.Day] = d
	}
	if err := rows.Err(); err != nil {
		return UsageDailyResponse{}, err
	}
	// Every day in the range is present, so a quiet day reads as zero rather
	// than as a gap a chart would interpolate across.
	out := UsageDailyResponse{From: from.Format(dayLayout), To: to.Format(dayLayout), Days: []UsageDay{}}
	for day := from; !day.After(to); day = day.AddDate(0, 0, 1) {
		key := day.Format(dayLayout)
		d, ok := byDay[key]
		if !ok {
			d = UsageDay{Day: key}
		}
		out.Days = append(out.Days, d)
	}
	return out, nil
}

// handleAdminUsageDaily serves GET /v1/admin/usage/daily. The ledger is
// invokable only by creditProxy's own service account; the gateway enforces
// the admin check before proxying here.
func (s *server) handleAdminUsageDaily(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	from, to, err := parseUsageRange(r.URL.Query().Get("from"), r.URL.Query().Get("to"), time.Now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	out, err := s.usageDaily(r.Context(), from, to, "")
	if err != nil {
		http.Error(w, "usage query failed", http.StatusInternalServerError)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, out)
}
