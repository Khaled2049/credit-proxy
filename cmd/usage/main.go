package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/kh1011/creditproxy/pkg/contracts"
	"github.com/kh1011/creditproxy/pkg/httpx"
	"github.com/kh1011/creditproxy/pkg/ids"
	"github.com/kh1011/creditproxy/pkg/version"
	redis "github.com/redis/go-redis/v9"
)

// platformDailyScript atomically increments the platform-wide daily request counter
// and returns 0 if the limit is already reached, or the new count if allowed.
// KEYS[1] = platform daily key, ARGV[1] = limit, ARGV[2] = TTL seconds
const platformDailyScript = `
local current = tonumber(redis.call("GET", KEYS[1]) or "0")
if current >= tonumber(ARGV[1]) then return 0 end
local new = redis.call("INCR", KEYS[1])
if new == 1 then redis.call("EXPIRE", KEYS[1], ARGV[2]) end
return new
`

// platformCreditsScript charges the platform-wide daily credit budget. A
// request cap alone stops bounding spend once one assistant run makes several
// tool-calling round trips, so capacity is metered in credits too. Returns -1
// when the charge would cross the limit, otherwise the new total.
// KEYS[1] = budget key, ARGV[1] = credits, ARGV[2] = limit, ARGV[3] = TTL seconds
const platformCreditsScript = `
local amount = tonumber(ARGV[1])
local current = tonumber(redis.call("GET", KEYS[1]) or "0")
if current + amount > tonumber(ARGV[2]) then return -1 end
local new = redis.call("INCRBY", KEYS[1], amount)
if new == amount then redis.call("EXPIRE", KEYS[1], ARGV[3]) end
return new
`

const reserveScript = `
local bal = redis.call("GET", KEYS[1])
if not bal then bal = "0" end
if tonumber(bal) < tonumber(ARGV[2]) then return {0, bal} end
redis.call("DECRBY", KEYS[1], ARGV[2])
redis.call("HSET", KEYS[2], "user_id", ARGV[1], "amount", ARGV[2], "status", "reserved", "platform_day", ARGV[4])
redis.call("EXPIRE", KEYS[2], ARGV[3])
return {1, redis.call("GET", KEYS[1])}
`

const commitScript = `
local status = redis.call("HGET", KEYS[2], "status")
if not status then return {-3, "missing"} end
if status == "committed" then return {1, "already_committed"} end
if status == "released" then return {-4, "already_released"} end
local platform_day = redis.call("HGET", KEYS[2], "platform_day") or ""
if platform_day ~= ARGV[3] then return {-5, "platform_day_mismatch"} end
local reserved = tonumber(redis.call("HGET", KEYS[2], "amount"))
local actual = tonumber(ARGV[2])
if actual > reserved then
  redis.call("DECRBY", KEYS[1], actual - reserved)
elseif reserved > actual then
  redis.call("INCRBY", KEYS[1], reserved - actual)
end
if platform_day ~= "" and actual ~= reserved then
  redis.call("INCRBY", KEYS[3], actual - reserved)
end
redis.call("HSET", KEYS[2], "status", "committed", "amount", actual, "reserved", reserved)
redis.call("EXPIRE", KEYS[2], 86400)
return {1, "committed", tostring(reserved), tostring(actual - reserved)}
`

const releaseScript = `
local status = redis.call("HGET", KEYS[2], "status")
if not status then return {-3, "missing"} end
if status == "released" then return {1, "already_released"} end
if status == "committed" then return {-4, "already_committed"} end
local platform_day = redis.call("HGET", KEYS[2], "platform_day") or ""
if platform_day ~= ARGV[2] then return {-5, "platform_day_mismatch"} end
local reserved = tonumber(redis.call("HGET", KEYS[2], "amount"))
redis.call("INCRBY", KEYS[1], reserved)
if platform_day ~= "" then
  redis.call("DECRBY", KEYS[3], reserved)
end
redis.call("HSET", KEYS[2], "status", "released")
redis.call("EXPIRE", KEYS[2], 86400)
return {1, "released", tostring(reserved)}
`

type server struct {
	rdb                 *redis.Client
	enablePurchase      bool
	platformDailyLimit  int64
	platformCreditLimit int64
	initialCredits      int64
	maxPurchasesPerDay  int64
}

func main() {
	addr := getenv("USAGE_ADDR", ":8081")
	redisURL := getenv("REDIS_URL", "redis://redis:6379")

	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		log.Fatalf("redis parse url: %v", err)
	}
	rdb := redis.NewClient(opts)
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		log.Fatalf("redis ping: %v", err)
	}

	platformDailyLimit, _ := strconv.ParseInt(getenv("PLATFORM_DAILY_REQUEST_LIMIT", "1400"), 10, 64)
	if platformDailyLimit <= 0 {
		platformDailyLimit = 1400
	}
	platformCreditLimit, _ := strconv.ParseInt(getenv("PLATFORM_DAILY_CREDIT_LIMIT", "150000"), 10, 64)
	if platformCreditLimit <= 0 {
		platformCreditLimit = 150000
	}
	initialCredits, _ := strconv.ParseInt(getenv("INITIAL_CREDITS", "10000"), 10, 64)
	if initialCredits < 0 {
		initialCredits = 0
	}
	maxPurchasesPerDay, _ := strconv.ParseInt(getenv("MAX_PURCHASES_PER_DAY_PER_USER", "3"), 10, 64)
	if maxPurchasesPerDay <= 0 {
		maxPurchasesPerDay = 3
	}
	s := &server{
		rdb: rdb,
		// Defaults to false on purpose: top-up mints credits with no payment step,
		// so an unconfigured deployment should not expose it. Every real
		// deployment sets this explicitly (terraform var enable_purchase_api,
		// .env.example, docker-compose) — the mismatch is fail-safe, not drift.
		enablePurchase:      strings.EqualFold(getenv("ENABLE_PURCHASE_API", "false"), "true"),
		platformDailyLimit:  platformDailyLimit,
		platformCreditLimit: platformCreditLimit,
		initialCredits:      initialCredits,
		maxPurchasesPerDay:  maxPurchasesPerDay,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/v1/credits/purchase", s.handlePurchase)
	mux.HandleFunc("/v1/reservations", s.handleCreateReservation)
	mux.HandleFunc("/v1/reservations/", s.handleReservationAction)
	mux.HandleFunc("/v1/users/", s.handleUserRoutes)

	log.Printf("usage listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"version": version.Version,
		"commit":  version.Commit,
	})
}

func (s *server) handlePurchase(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.enablePurchase {
		http.NotFound(w, r)
		return
	}
	var req contracts.PurchaseCreditsRequest
	if err := httpx.ReadJSON(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.UserID == "" || req.Credits <= 0 {
		http.Error(w, "user_id and positive credits are required", http.StatusBadRequest)
		return
	}
	// Per-user daily purchase cap, enforced atomically before any credits are
	// minted so a burst of concurrent requests can't slip past it. Keyed on the
	// UTC date; the counter auto-expires after 25h so it resets at midnight with
	// no cleanup. Reuses the same increment-under-limit script as the platform cap.
	today := time.Now().UTC().Format("2006-01-02")
	purchaseCountKey := "user:purchases:" + req.UserID + ":" + today
	allowed, err := s.rdb.Eval(
		r.Context(),
		platformDailyScript,
		[]string{purchaseCountKey},
		s.maxPurchasesPerDay,
		90000,
	).Int64()
	if err != nil {
		http.Error(w, "purchase limit check failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if allowed == 0 {
		http.Error(w, "daily purchase limit reached", http.StatusTooManyRequests)
		return
	}
	// Seed the free grant first so topping up before the first generation adds
	// to the initial balance rather than clobbering it (IncrBy on an absent key
	// would otherwise create it at just the purchased amount).
	if err := s.ensureInitialCredits(r.Context(), req.UserID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	key := userCreditsKey(req.UserID)
	newBal, err := s.rdb.IncrBy(r.Context(), key, req.Credits).Result()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{
		"user_id":           req.UserID,
		"purchased_credits": req.Credits,
		"available_credits": newBal,
	})
}

func (s *server) handleCreateReservation(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req contracts.ReservationRequest
	if err := httpx.ReadJSON(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.UserID == "" || req.EstimatedCredits <= 0 {
		http.Error(w, "user_id and positive estimated_credits are required", http.StatusBadRequest)
		return
	}
	if req.ReservationID == "" {
		req.ReservationID = ids.New("res")
	}
	if req.TTLSeconds <= 0 {
		req.TTLSeconds = 120
	}
	// Enforce global platform daily request cap before spending any per-user credits.
	//  TTL of 90000s (25h) ensures the key expires after reset.
	today := time.Now().UTC().Format("2006-01-02")
	capResult, err := s.rdb.Eval(
		r.Context(),
		platformDailyScript,
		[]string{"platform:daily:" + today},
		s.platformDailyLimit,
		90000,
	).Int64()
	if err != nil {
		http.Error(w, "platform quota check failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if capResult == 0 {
		http.Error(w, "platform daily request limit reached", http.StatusTooManyRequests)
		return
	}

	budget, err := s.rdb.Eval(
		r.Context(),
		platformCreditsScript,
		[]string{platformCreditsKey(today)},
		req.EstimatedCredits,
		s.platformCreditLimit,
		90000,
	).Int64()
	if err != nil {
		http.Error(w, "platform budget check failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if budget < 0 {
		http.Error(w, contracts.ErrPlatformBudgetExhausted, http.StatusTooManyRequests)
		return
	}

	// Grant free starting credits to new users (atomic: only sets if absent).
	if err := s.ensureInitialCredits(r.Context(), req.UserID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	keys := []string{userCreditsKey(req.UserID), reservationKey(req.ReservationID)}
	args := []any{req.UserID, req.EstimatedCredits, req.TTLSeconds, today}
	out, err := s.rdb.Eval(r.Context(), reserveScript, keys, args...).Result()
	if err != nil {
		s.adjustPlatformCredits(r.Context(), today, -req.EstimatedCredits)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	res, err := asIntSlice(out)
	if err != nil {
		s.adjustPlatformCredits(r.Context(), today, -req.EstimatedCredits)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(res) < 1 || res[0] != 1 {
		s.adjustPlatformCredits(r.Context(), today, -req.EstimatedCredits)
		http.Error(w, "insufficient credits", http.StatusPaymentRequired)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, contracts.ReservationResponse{
		ReservationID: req.ReservationID,
		UserID:        req.UserID,
		Reserved:      req.EstimatedCredits,
		Status:        "reserved",
	})
}

func (s *server) handleReservationAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1/reservations/")
	parts := strings.Split(path, "/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	resID, action := parts[0], parts[1]
	userID := r.URL.Query().Get("user_id")
	if userID == "" {
		http.Error(w, "user_id query param is required", http.StatusBadRequest)
		return
	}
	switch action {
	case "commit":
		s.handleCommit(w, r, userID, resID)
	case "release":
		s.handleRelease(w, r, userID, resID)
	default:
		http.NotFound(w, r)
	}
}

func (s *server) handleCommit(w http.ResponseWriter, r *http.Request, userID, resID string) {
	var req contracts.CommitReservationRequest
	if err := httpx.ReadJSON(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.ActualCredits < 0 {
		http.Error(w, "actual_credits cannot be negative", http.StatusBadRequest)
		return
	}
	day, err := s.reservationDay(r.Context(), resID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	keys := []string{userCreditsKey(userID), reservationKey(resID), platformCreditsKey(day)}
	args := []any{userID, req.ActualCredits, day}
	out, err := s.rdb.Eval(r.Context(), commitScript, keys, args...).Result()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	res, err := asStringSlice(out)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	code, _ := strconv.Atoi(res[0])
	if code < 0 {
		http.Error(w, "reservation not valid for commit", http.StatusConflict)
		return
	}
	if len(res) >= 4 {
		if overage, _ := strconv.ParseInt(res[3], 10, 64); overage > 0 {
			log.Printf("reservation overage res=%s user=%s reserved=%s overage=%d", resID, userID, res[2], overage)
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"reservation_id": resID, "status": "committed", "actual_credits": req.ActualCredits})
}

func (s *server) handleRelease(w http.ResponseWriter, r *http.Request, userID, resID string) {
	var req contracts.ReleaseReservationRequest
	_ = httpx.ReadJSON(r, &req)
	day, err := s.reservationDay(r.Context(), resID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	keys := []string{userCreditsKey(userID), reservationKey(resID), platformCreditsKey(day)}
	args := []any{userID, day}
	out, err := s.rdb.Eval(r.Context(), releaseScript, keys, args...).Result()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	res, err := asStringSlice(out)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	code, _ := strconv.Atoi(res[0])
	if code < 0 {
		http.Error(w, "reservation not valid for release", http.StatusConflict)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"reservation_id": resID, "status": "released", "reason": req.Reason})
}

func (s *server) handleUserRoutes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1/users/")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[1] != "balance" {
		http.NotFound(w, r)
		return
	}
	userID := parts[0]
	if userID == "" {
		http.NotFound(w, r)
		return
	}
	// Materialize the free grant so a brand-new user sees their real starting
	// balance instead of 0 (which would otherwise trip the low-credit top-up
	// nag in the UI before they've generated anything).
	if err := s.ensureInitialCredits(r.Context(), userID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	bal, err := s.rdb.Get(r.Context(), userCreditsKey(userID)).Int64()
	if err != nil && !errors.Is(err, redis.Nil) {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if errors.Is(err, redis.Nil) {
		bal = 0
	}
	httpx.WriteJSON(w, http.StatusOK, contracts.BalanceResponse{
		UserID:           userID,
		AvailableCredits: bal,
		EffectiveCredits: bal,
	})
}

// ensureInitialCredits grants a new user their free starting balance exactly
// once. SetNX is atomic and only writes when the key is absent, so an existing
// balance — including one legitimately spent down to 0 — is never overwritten.
// Calling this from every entry point that reads or mutates a balance keeps the
// lazy grant consistent: a user who checks their balance or tops up before their
// first generation still receives (and keeps) the free credits, instead of
// seeing 0 or having a top-up clobber the pending grant.
func (s *server) ensureInitialCredits(ctx context.Context, userID string) error {
	return s.rdb.SetNX(ctx, userCreditsKey(userID), s.initialCredits, 0).Err()
}

func (s *server) adjustPlatformCredits(ctx context.Context, day string, delta int64) {
	if delta == 0 {
		return
	}
	if err := s.rdb.IncrBy(ctx, platformCreditsKey(day), delta).Err(); err != nil {
		log.Printf("platform budget adjust failed day=%s delta=%d: %v", day, delta, err)
	}
}

func (s *server) reservationDay(ctx context.Context, resID string) (string, error) {
	day, err := s.rdb.HGet(ctx, reservationKey(resID), "platform_day").Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	return day, err
}

func platformCreditsKey(day string) string {
	return "platform:credits:" + day
}

func userCreditsKey(userID string) string {
	return "user:credits:" + userID
}

func reservationKey(reservationID string) string {
	return "reservation:" + reservationID
}

func asIntSlice(v any) ([]int64, error) {
	raw, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("unexpected eval result: %T", v)
	}
	out := make([]int64, 0, len(raw))
	for _, item := range raw {
		switch t := item.(type) {
		case int64:
			out = append(out, t)
		case string:
			iv, _ := strconv.ParseInt(t, 10, 64)
			out = append(out, iv)
		default:
			return nil, fmt.Errorf("unexpected eval value: %T", item)
		}
	}
	return out, nil
}

func asStringSlice(v any) ([]string, error) {
	raw, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("unexpected eval result: %T", v)
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		switch t := item.(type) {
		case int64:
			out = append(out, strconv.FormatInt(t, 10))
		case string:
			out = append(out, t)
		default:
			return nil, fmt.Errorf("unexpected eval value: %T", item)
		}
	}
	return out, nil
}

func getenv(key, fallback string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	return v
}
