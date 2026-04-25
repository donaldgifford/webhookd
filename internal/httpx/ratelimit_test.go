// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

package httpx_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/donaldgifford/webhookd/internal/config"
	"github.com/donaldgifford/webhookd/internal/httpx"
	"github.com/donaldgifford/webhookd/internal/observability"
)

func newRateLimitedHandler(cfg httpx.RateLimitConfig, m *observability.Metrics) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /webhook/{provider}",
		func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusAccepted)
		},
	)
	return httpx.RateLimit(cfg, m)(mux)
}

// TestRateLimit_AllowsThenRejects drives a tiny bucket past its burst
// and asserts (a) the first burst+1 requests are accepted,
// (b) the next request gets 429 with a Retry-After header, and
// (c) the rate-limited counter increments by exactly the rejection
// count. We use a bucket that never refills inside the test (rate=0.1)
// so timing slop on slow CI hosts cannot let extra requests through.
func TestRateLimit_AllowsThenRejects(t *testing.T) {
	_, m := observability.NewMetrics(&config.Config{})
	h := newRateLimitedHandler(httpx.RateLimitConfig{RPS: 0.1, Burst: 2}, m)

	doRequest := func() *httptest.ResponseRecorder {
		req, err := http.NewRequestWithContext(
			context.Background(), http.MethodPost,
			"/webhook/github", http.NoBody,
		)
		if err != nil {
			t.Fatalf("build req: %v", err)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		return rr
	}

	// First two requests fit in the burst.
	for range 2 {
		if rr := doRequest(); rr.Code != http.StatusAccepted {
			t.Errorf("burst request status = %d, want 202", rr.Code)
		}
	}

	rr := doRequest()
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("over-limit status = %d, want 429", rr.Code)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Error("expected Retry-After header on 429")
	}

	got := testutil.ToFloat64(m.HTTPRateLimited.WithLabelValues("github"))
	if got != 1 {
		t.Errorf("rate_limited counter = %v, want 1", got)
	}
}

// TestRateLimit_BypassesUnmatchedRoutes covers the admin-probe and
// 404 paths: routes with no {provider} path-value must not get
// rate-limited, otherwise a flood of 404s could lock out healthchecks.
func TestRateLimit_BypassesUnmatchedRoutes(t *testing.T) {
	_, m := observability.NewMetrics(&config.Config{})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := httpx.RateLimit(httpx.RateLimitConfig{RPS: 0.1, Burst: 0}, m)(mux)

	for range 5 {
		req, err := http.NewRequestWithContext(
			context.Background(), http.MethodGet, "/healthz", http.NoBody,
		)
		if err != nil {
			t.Fatalf("build req: %v", err)
		}
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("/healthz status = %d, want 200", rr.Code)
		}
	}
}
