package httpx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPostJSON_NonSuccessReturnsUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte("insufficient credits"))
	}))
	defer srv.Close()

	var out map[string]any
	err := PostJSON(context.Background(), srv.Client(), srv.URL, map[string]string{"a": "b"}, &out, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var upErr *UpstreamError
	if !errors.As(err, &upErr) {
		t.Fatalf("expected *UpstreamError, got %T: %v", err, err)
	}
	if upErr.StatusCode != http.StatusPaymentRequired {
		t.Errorf("StatusCode = %d, want %d", upErr.StatusCode, http.StatusPaymentRequired)
	}
	if upErr.Body != "insufficient credits" {
		t.Errorf("Body = %q, want %q", upErr.Body, "insufficient credits")
	}
}

func TestPostJSON_ErrorStringUnchanged(t *testing.T) {
	// Callers that still grep the error string (e.g. log lines) must keep working.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("platform daily request limit reached"))
	}))
	defer srv.Close()

	err := PostJSON[any, any](context.Background(), srv.Client(), srv.URL, nil, nil, nil)
	want := "upstream status 429: platform daily request limit reached"
	if err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
}

func TestPostJSON_SuccessDecodesBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	var out struct {
		OK bool `json:"ok"`
	}
	if err := PostJSON(context.Background(), srv.Client(), srv.URL, map[string]string{}, &out, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !out.OK {
		t.Error("expected out.OK to be true")
	}
}

func TestPostJSON_NetworkErrorIsNotUpstreamError(t *testing.T) {
	// A connection failure never reached the server, so it must NOT be
	// classified as an UpstreamError (there is no real status code).
	err := PostJSON[any, any](context.Background(), http.DefaultClient, "http://127.0.0.1:1/unreachable", nil, nil, nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var upErr *UpstreamError
	if errors.As(err, &upErr) {
		t.Fatalf("network error should not be an *UpstreamError, got %+v", upErr)
	}
}
