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

	"github.com/kh1011/creditproxy/pkg/contracts"
	"github.com/kh1011/creditproxy/pkg/httpx"
	"github.com/kh1011/creditproxy/pkg/ids"
	"github.com/kh1011/creditproxy/pkg/version"
	redis "github.com/redis/go-redis/v9"
)

const reserveScript = `
local bal = redis.call("GET", KEYS[1])
if not bal then bal = "0" end
if tonumber(bal) < tonumber(ARGV[2]) then return {0, bal} end
redis.call("DECRBY", KEYS[1], ARGV[2])
redis.call("HSET", KEYS[2], "user_id", ARGV[1], "amount", ARGV[2], "status", "reserved")
redis.call("EXPIRE", KEYS[2], ARGV[3])
return {1, redis.call("GET", KEYS[1])}
`

const commitScript = `
local status = redis.call("HGET", KEYS[2], "status")
if not status then return {-3, "missing"} end
if status == "committed" then return {1, "already_committed"} end
if status == "released" then return {-4, "already_released"} end
local reserved = tonumber(redis.call("HGET", KEYS[2], "amount"))
local actual = tonumber(ARGV[2])
if actual > reserved then
  local extra = actual - reserved
  local bal = tonumber(redis.call("GET", KEYS[1]) or "0")
  if bal < extra then return {-2, "insufficient_for_reconcile"} end
  redis.call("DECRBY", KEYS[1], extra)
elseif reserved > actual then
  local refund = reserved - actual
  redis.call("INCRBY", KEYS[1], refund)
end
redis.call("HSET", KEYS[2], "status", "committed", "amount", actual)
redis.call("EXPIRE", KEYS[2], 86400)
return {1, "committed"}
`

const releaseScript = `
local status = redis.call("HGET", KEYS[2], "status")
if not status then return {-3, "missing"} end
if status == "released" then return {1, "already_released"} end
if status == "committed" then return {-4, "already_committed"} end
local reserved = tonumber(redis.call("HGET", KEYS[2], "amount"))
redis.call("INCRBY", KEYS[1], reserved)
redis.call("HSET", KEYS[2], "status", "released")
redis.call("EXPIRE", KEYS[2], 86400)
return {1, "released"}
`

type server struct {
	rdb *redis.Client
}

func main() {
	addr := getenv("USAGE_ADDR", ":8081")
	redisAddr := getenv("REDIS_ADDR", "redis:6379")

	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		log.Fatalf("redis ping: %v", err)
	}

	s := &server{rdb: rdb}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
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
	var req contracts.PurchaseCreditsRequest
	if err := httpx.ReadJSON(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.UserID == "" || req.Credits <= 0 {
		http.Error(w, "user_id and positive credits are required", http.StatusBadRequest)
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
	keys := []string{userCreditsKey(req.UserID), reservationKey(req.ReservationID)}
	args := []any{req.UserID, req.EstimatedCredits, req.TTLSeconds}
	out, err := s.rdb.Eval(r.Context(), reserveScript, keys, args...).Result()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	res, err := asIntSlice(out)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(res) < 1 || res[0] != 1 {
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
	keys := []string{userCreditsKey(userID), reservationKey(resID)}
	args := []any{userID, req.ActualCredits}
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
	if code == -2 {
		http.Error(w, "insufficient credits for reconciliation", http.StatusPaymentRequired)
		return
	}
	if code < 0 {
		http.Error(w, "reservation not valid for commit", http.StatusConflict)
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"reservation_id": resID, "status": "committed", "actual_credits": req.ActualCredits})
}

func (s *server) handleRelease(w http.ResponseWriter, r *http.Request, userID, resID string) {
	var req contracts.ReleaseReservationRequest
	_ = httpx.ReadJSON(r, &req)
	keys := []string{userCreditsKey(userID), reservationKey(resID)}
	args := []any{userID}
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
