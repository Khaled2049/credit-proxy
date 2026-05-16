package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseMode(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want Mode
	}{
		{name: "production", raw: "production", want: ModeProduction},
		{name: "dev strict", raw: "dev_strict", want: ModeDevStrict},
		{name: "unknown defaults to dev", raw: "staging", want: ModeDev},
		{name: "empty defaults to dev", raw: "", want: ModeDev},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseMode(tc.raw)
			if got != tc.want {
				t.Fatalf("expected mode %q, got %q", tc.want, got)
			}
		})
	}
}

func TestResolveBillingUserDevUsesBodyUserID(t *testing.T) {
	v := NewVerifier(Config{Mode: ModeDev})
	req := httptest.NewRequest(http.MethodPost, "http://localhost/v1/generate", nil)

	uid, err := v.ResolveBillingUser(context.Background(), req, "user-123")
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if uid != "user-123" {
		t.Fatalf("expected uid %q, got %q", "user-123", uid)
	}
}

func TestResolveBillingUserDevRequiresBodyUserIDWhenFirebaseMissing(t *testing.T) {
	v := NewVerifier(Config{Mode: ModeDev})
	req := httptest.NewRequest(http.MethodPost, "http://localhost/v1/generate", nil)

	_, err := v.ResolveBillingUser(context.Background(), req, "")
	if err == nil {
		t.Fatal("expected auth error")
	}
	if err.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected status %d, got %d", http.StatusBadRequest, err.StatusCode)
	}
}

func TestResolveBillingUserDevStrictRequiresFirebase(t *testing.T) {
	v := NewVerifier(Config{Mode: ModeDevStrict})
	req := httptest.NewRequest(http.MethodPost, "http://localhost/v1/generate", nil)

	_, err := v.ResolveBillingUser(context.Background(), req, "user-123")
	if err == nil {
		t.Fatal("expected auth error")
	}
	if err.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected status %d, got %d", http.StatusUnauthorized, err.StatusCode)
	}
}

func TestResolveBillingUserProductionRequiresBearer(t *testing.T) {
	v := NewVerifier(Config{Mode: ModeProduction, GCPAudience: "https://example.com"})
	req := httptest.NewRequest(http.MethodPost, "https://example.com/v1/generate", nil)

	_, err := v.ResolveBillingUser(context.Background(), req, "user-123")
	if err == nil {
		t.Fatal("expected auth error")
	}
	if err.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected status %d, got %d", http.StatusUnauthorized, err.StatusCode)
	}
}

func TestResolveBillingUserProductionRequiresAudience(t *testing.T) {
	v := NewVerifier(Config{Mode: ModeProduction})
	req := httptest.NewRequest(http.MethodPost, "/v1/generate", nil)
	req.Host = ""
	req.Header.Set("Authorization", "Bearer test-token")

	_, err := v.ResolveBillingUser(context.Background(), req, "user-123")
	if err == nil {
		t.Fatal("expected auth error")
	}
	if err.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected status %d, got %d", http.StatusInternalServerError, err.StatusCode)
	}
}

func TestAudienceFromRequest(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "https://credit-proxy.run.app/v1/generate", nil)
	if got := audienceFromRequest(req); got != "https://credit-proxy.run.app" {
		t.Fatalf("expected %q, got %q", "https://credit-proxy.run.app", got)
	}
}

func TestResolveBillingUserIDProductionRejectsMismatchedUserID(t *testing.T) {
	_, err := resolveBillingUserID("body-user", "firebase-user")
	if err == nil {
		t.Fatal("expected auth error")
	}
	if err.StatusCode != http.StatusForbidden {
		t.Fatalf("expected status %d, got %d", http.StatusForbidden, err.StatusCode)
	}
}

func TestMatchesIdentity(t *testing.T) {
	allowed := []string{"service@project.iam.gserviceaccount.com", "12345-subject"}
	if !matchesIdentity(allowed, "service@project.iam.gserviceaccount.com", "") {
		t.Fatal("expected email identity to match")
	}
	if !matchesIdentity(allowed, "", "12345-subject") {
		t.Fatal("expected subject identity to match")
	}
	if matchesIdentity(allowed, "other@project.iam.gserviceaccount.com", "other-subject") {
		t.Fatal("expected mismatched identities to be rejected")
	}
}
