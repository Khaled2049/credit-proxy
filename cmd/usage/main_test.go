package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
	return &server{rdb: rdb, platformDailyLimit: dailyLimit}, mr
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

func TestPlatformDailyCap_DoesNotConsumePerUserCreditsWhenBlocked(t *testing.T) {
	s, _ := newTestServer(t, 0) // limit=0 → every request blocked immediately
	s.rdb.Set(t.Context(), userCreditsKey("u1"), 500, 0)

	postReservation(t, s, "u1", 100)

	bal, _ := s.rdb.Get(t.Context(), userCreditsKey("u1")).Int64()
	if bal != 500 {
		t.Fatalf("per-user balance should be untouched when platform cap blocks; got %d", bal)
	}
}
