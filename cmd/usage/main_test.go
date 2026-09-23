package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/kh1011/creditproxy/pkg/contracts"
	redis "github.com/redis/go-redis/v9"
)

// newTestServer starts a miniredis instance and returns a server wired to it.
// Callers must call mr.Close() when done.
func newTestServer(t *testing.T, dailyLimit int64) (*server, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	// Default the per-user purchase cap and the platform credit budget high so
	// tests that don't exercise them aren't accidentally throttled; the tests
	// for those caps set them explicitly.
	return &server{rdb: rdb, platformDailyLimit: dailyLimit, platformCreditLimit: 1_000_000, maxPurchasesPerDay: 100}, mr
}

func getBalance(t *testing.T, s *server, userID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/users/"+userID+"/balance", nil)
	w := httptest.NewRecorder()
	s.handleUserRoutes(w, req)
	return w
}

func postPurchase(t *testing.T, s *server, userID string, credits int64) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(contracts.PurchaseCreditsRequest{UserID: userID, Credits: credits})
	req := httptest.NewRequest(http.MethodPost, "/v1/credits/purchase", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handlePurchase(w, req)
	return w
}

func postReservation(t *testing.T, s *server, userID string, credits int64) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(contracts.ReservationRequest{
		UserID:           userID,
		EstimatedCredits: credits,
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/reservations", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleCreateReservation(w, req)
	return w
}

// --- script content tests (keep existing pattern) ---

func TestReserveScriptContainsAtomicDebit(t *testing.T) {
	if !strings.Contains(reserveScript, `DECRBY`) {
		t.Fatal("reserve script should atomically debit balance")
	}
	if !strings.Contains(reserveScript, `EXPIRE`) {
		t.Fatal("reserve script should set ttl")
	}
}

func TestCommitScriptContainsReconciliation(t *testing.T) {
	if !strings.Contains(commitScript, `if actual > reserved`) {
		t.Fatal("commit script should handle extra charge path")
	}
	if !strings.Contains(commitScript, `elseif reserved > actual`) {
		t.Fatal("commit script should handle refund path")
	}
}

func TestPlatformDailyScriptContainsGuard(t *testing.T) {
	if !strings.Contains(platformDailyScript, `current >= tonumber(ARGV[1])`) {
		t.Fatal("platform daily script should block when limit reached")
	}
	if !strings.Contains(platformDailyScript, `EXPIRE`) {
		t.Fatal("platform daily script should set key TTL for auto-cleanup")
	}
}

// --- platform daily cap integration tests ---

func TestPlatformDailyCap_AllowsRequestsUnderLimit(t *testing.T) {
	s, _ := newTestServer(t, 3)
	// Pre-seed credits so per-user reservation succeeds.
	s.rdb.Set(t.Context(), userCreditsKey("u1"), 100000, 0)

	for i := range 3 {
		w := postReservation(t, s, "u1", 100)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: want 200, got %d: %s", i+1, w.Code, w.Body.String())
		}
	}
}

func TestPlatformDailyCap_BlocksAtLimit(t *testing.T) {
	s, _ := newTestServer(t, 2)
	s.rdb.Set(t.Context(), userCreditsKey("u1"), 100000, 0)

	postReservation(t, s, "u1", 100)
	postReservation(t, s, "u1", 100)

	w := postReservation(t, s, "u1", 100)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("want 429 when cap exceeded, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "platform daily request limit reached") {
		t.Fatalf("unexpected error body: %s", w.Body.String())
	}
}

func TestPlatformDailyCap_DifferentUsersShareOneCap(t *testing.T) {
	s, _ := newTestServer(t, 2)
	s.rdb.Set(t.Context(), userCreditsKey("alice"), 100000, 0)
	s.rdb.Set(t.Context(), userCreditsKey("bob"), 100000, 0)

	postReservation(t, s, "alice", 100)
	postReservation(t, s, "bob", 100)

	// Third request from any user should be blocked — cap is shared.
	w := postReservation(t, s, "alice", 100)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("cap should be platform-wide, not per-user: got %d", w.Code)
	}
}

func TestPlatformDailyCap_ResetsNextDay(t *testing.T) {
	s, mr := newTestServer(t, 1)
	s.rdb.Set(t.Context(), userCreditsKey("u1"), 100000, 0)

	// Exhaust today's cap.
	postReservation(t, s, "u1", 100)
	w := postReservation(t, s, "u1", 100)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected cap hit, got %d", w.Code)
	}

	// Fast-forward miniredis clock by 25h so today's key expires.
	mr.FastForward(25 * time.Hour)

	// Next day's key doesn't exist yet — request should succeed.
	w = postReservation(t, s, "u1", 100)
	if w.Code != http.StatusOK {
		t.Fatalf("want 200 after daily reset, got %d: %s", w.Code, w.Body.String())
	}
}

// --- new-user balance edge cases ---

// decodeBalance pulls available_credits out of a BalanceResponse recorder.
func decodeBalance(t *testing.T, w *httptest.ResponseRecorder) int64 {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("balance: want 200, got %d: %s", w.Code, w.Body.String())
	}
	var out contracts.BalanceResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode balance: %v", err)
	}
	return out.AvailableCredits
}

func TestBalance_BrandNewUserReflectsInitialGrant(t *testing.T) {
	s, _ := newTestServer(t, 10)
	s.initialCredits = 10000

	// A user who has never generated should see their free starting balance,
	// not 0 (which would wrongly trip the low-credit top-up nag).
	if got := decodeBalance(t, getBalance(t, s, "newbie")); got != 10000 {
		t.Fatalf("new-user balance = %d, want 10000", got)
	}
}

func TestBalance_DoesNotRegrantSpentDownBalance(t *testing.T) {
	s, _ := newTestServer(t, 10)
	s.initialCredits = 10000
	// User exists and has legitimately spent everything.
	s.rdb.Set(t.Context(), userCreditsKey("spent"), 0, 0)

	if got := decodeBalance(t, getBalance(t, s, "spent")); got != 0 {
		t.Fatalf("spent-down balance = %d, want 0 (must not re-grant)", got)
	}
}

func TestBalance_EmptyUserIDIsNotFound(t *testing.T) {
	s, _ := newTestServer(t, 10)
	s.initialCredits = 10000

	req := httptest.NewRequest(http.MethodGet, "/v1/users//balance", nil)
	w := httptest.NewRecorder()
	s.handleUserRoutes(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("empty user id: want 404, got %d", w.Code)
	}
	// The junk key must not have been seeded.
	if s.rdb.Exists(t.Context(), userCreditsKey("")).Val() != 0 {
		t.Fatal("empty-user-id balance must not create a credits key")
	}
}

func TestPurchase_BeforeFirstGenerationAddsToInitialGrant(t *testing.T) {
	s, _ := newTestServer(t, 10)
	s.enablePurchase = true
	s.initialCredits = 10000

	// Top up before ever generating: result must be initial + purchase, not
	// just the purchased amount.
	if got := decodeBalance(t, postPurchase(t, s, "u1", 50000)); got != 60000 {
		t.Fatalf("balance after top-up = %d, want 60000 (10000 grant + 50000)", got)
	}
	if got := decodeBalance(t, getBalance(t, s, "u1")); got != 60000 {
		t.Fatalf("subsequent balance = %d, want 60000", got)
	}
}

func TestPurchase_DailyCapBlocksAfterLimit(t *testing.T) {
	s, _ := newTestServer(t, 10)
	s.enablePurchase = true
	s.initialCredits = 0
	s.maxPurchasesPerDay = 3

	// First 3 purchases succeed.
	for i := 1; i <= 3; i++ {
		w := postPurchase(t, s, "u1", 200)
		if w.Code != http.StatusOK {
			t.Fatalf("purchase %d: want 200, got %d: %s", i, w.Code, w.Body.String())
		}
	}

	// 4th is rejected with 429 and does NOT mint (balance stays 600).
	w := postPurchase(t, s, "u1", 200)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("4th purchase: want 429, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "daily purchase limit reached") {
		t.Fatalf("unexpected error body: %s", w.Body.String())
	}
	if got := decodeBalance(t, getBalance(t, s, "u1")); got != 600 {
		t.Fatalf("balance after blocked purchase = %d, want 600 (unchanged)", got)
	}
}

func TestPurchase_DailyCapIsPerUser(t *testing.T) {
	s, _ := newTestServer(t, 10)
	s.enablePurchase = true
	s.initialCredits = 0
	s.maxPurchasesPerDay = 1

	if w := postPurchase(t, s, "alice", 200); w.Code != http.StatusOK {
		t.Fatalf("alice first purchase: want 200, got %d", w.Code)
	}
	// Alice is now capped, but Bob is unaffected.
	if w := postPurchase(t, s, "alice", 200); w.Code != http.StatusTooManyRequests {
		t.Fatalf("alice second purchase: want 429, got %d", w.Code)
	}
	if w := postPurchase(t, s, "bob", 200); w.Code != http.StatusOK {
		t.Fatalf("bob first purchase: want 200 (per-user cap), got %d", w.Code)
	}
}

func TestPurchase_DoesNotRegrantForExistingUser(t *testing.T) {
	s, _ := newTestServer(t, 10)
	s.enablePurchase = true
	s.initialCredits = 10000
	// Existing user who has spent down to 500.
	s.rdb.Set(t.Context(), userCreditsKey("u1"), 500, 0)

	if got := decodeBalance(t, postPurchase(t, s, "u1", 10000)); got != 10500 {
		t.Fatalf("balance after top-up = %d, want 10500 (500 + 10000, no re-grant)", got)
	}
}

func TestPlatformDailyCap_DoesNotConsumePerUserCreditsWhenBlocked(t *testing.T) {
	s, _ := newTestServer(t, 0) // limit=0 → every request blocked immediately
	s.rdb.Set(t.Context(), userCreditsKey("u1"), 500, 0)

	postReservation(t, s, "u1", 100)

	bal, _ := s.rdb.Get(t.Context(), userCreditsKey("u1")).Int64()
	if bal != 500 {
		t.Fatalf("per-user balance should be untouched when platform cap blocks; got %d", bal)
	}
}

func TestPlatformCreditBudget_ConcurrentReservationsStopAtLimit(t *testing.T) {
	s, _ := newTestServer(t, 100)
	s.platformCreditLimit = 100
	s.initialCredits = 1000

	const requests = 20
	var successes atomic.Int64
	var exhausted atomic.Int64
	var wg sync.WaitGroup
	for i := range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := postReservation(t, s, fmt.Sprintf("user-%d", i), 10)
			switch w.Code {
			case http.StatusOK:
				successes.Add(1)
			case http.StatusTooManyRequests:
				if !strings.Contains(w.Body.String(), contracts.ErrPlatformBudgetExhausted) {
					t.Errorf("unexpected 429 body: %s", w.Body.String())
				}
				exhausted.Add(1)
			default:
				t.Errorf("unexpected status %d: %s", w.Code, w.Body.String())
			}
		}()
	}
	wg.Wait()

	if got := successes.Load(); got != 10 {
		t.Fatalf("successful reservations = %d, want 10", got)
	}
	if got := exhausted.Load(); got != 10 {
		t.Fatalf("budget refusals = %d, want 10", got)
	}
	day := time.Now().UTC().Format("2006-01-02")
	if got := s.rdb.Get(t.Context(), platformCreditsKey(day)).Val(); got != "100" {
		t.Fatalf("platform credit counter = %s, want 100", got)
	}
}

func TestPlatformCreditBudget_ReconcilesReservationDay(t *testing.T) {
	s, _ := newTestServer(t, 10)
	s.platformCreditLimit = 1000
	s.initialCredits = 1000

	w := postReservation(t, s, "u1", 100)
	if w.Code != http.StatusOK {
		t.Fatalf("reserve: want 200, got %d: %s", w.Code, w.Body.String())
	}
	var reservation contracts.ReservationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &reservation); err != nil {
		t.Fatal(err)
	}

	originalDay := s.rdb.HGet(t.Context(), reservationKey(reservation.ReservationID), "platform_day").Val()
	recordedDay := "2000-01-01"
	s.rdb.HSet(t.Context(), reservationKey(reservation.ReservationID), "platform_day", recordedDay)
	s.rdb.Set(t.Context(), platformCreditsKey(recordedDay), 100, 25*time.Hour)

	body := strings.NewReader(`{"reason":"test"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/reservations/"+reservation.ReservationID+"/release?user_id=u1", body)
	rr := httptest.NewRecorder()
	s.handleReservationAction(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("release: want 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := s.rdb.Get(t.Context(), platformCreditsKey(recordedDay)).Val(); got != "0" {
		t.Fatalf("reservation-day budget = %s, want 0", got)
	}
	if got := s.rdb.Get(t.Context(), platformCreditsKey(originalDay)).Val(); got != "100" {
		t.Fatalf("current-day budget = %s, want 100", got)
	}
}

func reserveFor(t *testing.T, s *server, userID string, credits int64) string {
	t.Helper()
	w := postReservation(t, s, userID, credits)
	if w.Code != http.StatusOK {
		t.Fatalf("reserve: want 200, got %d: %s", w.Code, w.Body.String())
	}
	var reservation contracts.ReservationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &reservation); err != nil {
		t.Fatal(err)
	}
	return reservation.ReservationID
}

func commitFor(t *testing.T, s *server, userID, resID string, actual int64) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(contracts.CommitReservationRequest{ActualCredits: actual})
	req := httptest.NewRequest(http.MethodPost, "/v1/reservations/"+resID+"/commit?user_id="+userID, bytes.NewReader(body))
	w := httptest.NewRecorder()
	s.handleReservationAction(w, req)
	return w
}

func TestCommit_OverageBeyondBalanceIsChargedAsDebt(t *testing.T) {
	s, _ := newTestServer(t, 10)
	s.initialCredits = 100
	day := time.Now().UTC().Format("2006-01-02")

	resID := reserveFor(t, s, "u1", 80)
	if w := commitFor(t, s, "u1", resID, 150); w.Code != http.StatusOK {
		t.Fatalf("commit: want 200, got %d: %s", w.Code, w.Body.String())
	}
	if got, _ := s.rdb.Get(t.Context(), userCreditsKey("u1")).Int64(); got != -50 {
		t.Fatalf("balance = %d, want -50", got)
	}
	if got := s.rdb.Get(t.Context(), platformCreditsKey(day)).Val(); got != "150" {
		t.Fatalf("platform credits = %s, want 150", got)
	}
	if w := postReservation(t, s, "u1", 1); w.Code != http.StatusPaymentRequired {
		t.Fatalf("reserve while in debt: want 402, got %d", w.Code)
	}
	if w := commitFor(t, s, "u1", resID, 150); w.Code != http.StatusOK {
		t.Fatalf("repeat commit: want 200, got %d", w.Code)
	}
	if got, _ := s.rdb.Get(t.Context(), userCreditsKey("u1")).Int64(); got != -50 {
		t.Fatalf("balance after repeat commit = %d, want -50", got)
	}
}

func TestCommit_RefundReconcilesPlatformBudget(t *testing.T) {
	s, _ := newTestServer(t, 10)
	s.initialCredits = 1000
	day := time.Now().UTC().Format("2006-01-02")

	resID := reserveFor(t, s, "u1", 300)
	if w := commitFor(t, s, "u1", resID, 40); w.Code != http.StatusOK {
		t.Fatalf("commit: want 200, got %d: %s", w.Code, w.Body.String())
	}
	if got, _ := s.rdb.Get(t.Context(), userCreditsKey("u1")).Int64(); got != 960 {
		t.Fatalf("balance = %d, want 960", got)
	}
	if got := s.rdb.Get(t.Context(), platformCreditsKey(day)).Val(); got != "40" {
		t.Fatalf("platform credits = %s, want 40", got)
	}
}

func TestCommit_ConcurrentOveragesSettleExactly(t *testing.T) {
	s, _ := newTestServer(t, 100)
	s.initialCredits = 1000
	day := time.Now().UTC().Format("2006-01-02")

	const requests = 10
	resIDs := make([]string, requests)
	for i := range resIDs {
		resIDs[i] = reserveFor(t, s, "u1", 100)
	}
	var wg sync.WaitGroup
	for _, resID := range resIDs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if w := commitFor(t, s, "u1", resID, 130); w.Code != http.StatusOK {
				t.Errorf("commit: want 200, got %d: %s", w.Code, w.Body.String())
			}
		}()
	}
	wg.Wait()

	if got, _ := s.rdb.Get(t.Context(), userCreditsKey("u1")).Int64(); got != -300 {
		t.Fatalf("balance = %d, want -300", got)
	}
	if got := s.rdb.Get(t.Context(), platformCreditsKey(day)).Val(); got != "1300" {
		t.Fatalf("platform credits = %s, want 1300", got)
	}
}
