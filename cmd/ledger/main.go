package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/kh1011/creditproxy/pkg/contracts"
	"github.com/kh1011/creditproxy/pkg/httpx"
	"github.com/kh1011/creditproxy/pkg/version"
	_ "github.com/lib/pq"
)

const schemaSQL = `
CREATE TABLE IF NOT EXISTS ledger_events (
  id BIGSERIAL PRIMARY KEY,
  idempotency_key TEXT NOT NULL UNIQUE,
  user_id TEXT NOT NULL,
  reservation_id TEXT,
  event_type TEXT NOT NULL,
  credits BIGINT NOT NULL,
  payload JSONB NOT NULL DEFAULT '{}'::jsonb,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_ledger_events_user_created_at ON ledger_events(user_id, created_at DESC);
`

type server struct {
	db *sql.DB
}

func main() {
	addr := getenv("LEDGER_ADDR", ":8083")
	dsn := getenv("POSTGRES_DSN", "postgres://postgres:postgres@postgres:5432/creditproxy?sslmode=disable")

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	db.SetConnMaxLifetime(5 * time.Minute)
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(10)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		log.Fatalf("ping db: %v", err)
	}
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		log.Fatalf("ensure schema: %v", err)
	}

	s := &server{db: db}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/v1/events", s.handleEvents)
	mux.HandleFunc("/v1/users/", s.handleUserRoutes)

	log.Printf("ledger listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func (s *server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, map[string]string{
		"status":  "ok",
		"version": version.Version,
		"commit":  version.Commit,
	})
}

func (s *server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req contracts.LedgerEventRequest
	if err := httpx.ReadJSON(r, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.IdempotencyKey == "" || req.UserID == "" || req.EventType == "" {
		http.Error(w, "idempotency_key, user_id, and event_type are required", http.StatusBadRequest)
		return
	}
	payload, err := json.Marshal(req.Payload)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	row := s.db.QueryRowContext(r.Context(), `
INSERT INTO ledger_events (idempotency_key, user_id, reservation_id, event_type, credits, payload)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (idempotency_key) DO UPDATE SET idempotency_key = EXCLUDED.idempotency_key
RETURNING id, idempotency_key, user_id, COALESCE(reservation_id,''), event_type, credits, created_at
`, req.IdempotencyKey, req.UserID, req.ReservationID, req.EventType, req.Credits, payload)

	var resp contracts.LedgerEventResponse
	var createdAt time.Time
	if err := row.Scan(&resp.ID, &resp.IdempotencyKey, &resp.UserID, &resp.ReservationID, &resp.EventType, &resp.Credits, &createdAt); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
	httpx.WriteJSON(w, http.StatusOK, resp)
}

func (s *server) handleUserRoutes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1/users/")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[1] != "ledger" {
		http.NotFound(w, r)
		return
	}
	userID := parts[0]
	rows, err := s.db.QueryContext(r.Context(), `
SELECT id, idempotency_key, user_id, COALESCE(reservation_id,''), event_type, credits, created_at
FROM ledger_events
WHERE user_id = $1
ORDER BY id DESC
LIMIT 100
`, userID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	events := make([]contracts.LedgerEventResponse, 0)
	for rows.Next() {
		var e contracts.LedgerEventResponse
		var createdAt time.Time
		if err := rows.Scan(&e.ID, &e.IdempotencyKey, &e.UserID, &e.ReservationID, &e.EventType, &e.Credits, &createdAt); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		e.CreatedAt = createdAt.UTC().Format(time.RFC3339Nano)
		events = append(events, e)
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"events": events})
}

func getenv(key, fallback string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	return v
}
