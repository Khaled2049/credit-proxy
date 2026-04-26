package main

import "testing"

func TestGetenvFallback(t *testing.T) {
	t.Setenv("X_TEST_ENV", "")
	if got := getenv("X_TEST_ENV", "fallback"); got != "fallback" {
		t.Fatalf("expected fallback, got %q", got)
	}
}
