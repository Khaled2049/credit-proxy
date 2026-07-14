package main

import (
	"errors"
	"net/http"
	"testing"

	"github.com/kh1011/creditproxy/pkg/httpx"
)

func TestClassifyReservationStatus(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"real insufficient credits", &httpx.UpstreamError{StatusCode: 402, Body: "insufficient credits"}, 402},
		{"platform daily cap", &httpx.UpstreamError{StatusCode: 429, Body: "platform daily request limit reached"}, 429},
		{"usage service internal error is not the user's fault", &httpx.UpstreamError{StatusCode: 500, Body: "redis: connection refused"}, http.StatusServiceUnavailable},
		{"malformed reservation request is a gateway bug, not user's fault", &httpx.UpstreamError{StatusCode: 400, Body: "user_id and positive estimated_credits are required"}, http.StatusServiceUnavailable},
		{"network error, no status code available", errors.New("dial tcp: connection refused"), http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyReservationStatus(tc.err); got != tc.want {
				t.Errorf("classifyReservationStatus(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

func TestClassifyGenerationStatus(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"bad byok config", &httpx.UpstreamError{StatusCode: 400, Body: "invalid byok config"}, 400},
		{"provider auth failure", &httpx.UpstreamError{StatusCode: 401, Body: "generation failed (ref llm_1)"}, 401},
		{"provider rate limited", &httpx.UpstreamError{StatusCode: 429, Body: "generation failed (ref llm_2)"}, 429},
		{"model not found", &httpx.UpstreamError{StatusCode: 404, Body: "generation failed (ref llm_3)"}, 404},
		{"unclassified provider failure", &httpx.UpstreamError{StatusCode: 502, Body: "generation failed (ref llm_4)"}, http.StatusBadGateway},
		{"network error, no status code available", errors.New("dial tcp: connection refused"), http.StatusBadGateway},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyGenerationStatus(tc.err); got != tc.want {
				t.Errorf("classifyGenerationStatus(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}
