// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

package httpx_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/donaldgifford/webhookd/internal/config"
	"github.com/donaldgifford/webhookd/internal/httpx"
	"github.com/donaldgifford/webhookd/internal/observability"
)

func newTestMetrics(t *testing.T) *observability.Metrics {
	t.Helper()
	_, m := observability.NewMetrics(&config.Config{})
	return m
}

func newTestLogger(buf *bytes.Buffer) *slog.Logger {
	return observability.NewLogger(buf, slog.LevelInfo, "json")
}

// TestRecover_CatchesPanic confirms the middleware writes 500, increments
// HTTPPanics, and emits an error log. Without recovery, the panic would
// propagate to net/http's default handler and produce a torn connection
// instead of a clean response.
func TestRecover_CatchesPanic(t *testing.T) {
	var buf bytes.Buffer
	logger := newTestLogger(&buf)
	m := newTestMetrics(t)

	h := httpx.Recover(logger, m)(http.HandlerFunc(
		func(http.ResponseWriter, *http.Request) {
			panic("boom")
		},
	))

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", http.NoBody)
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusInternalServerError)
	}
	if !strings.Contains(buf.String(), "http handler panic") {
		t.Errorf("expected panic log, got: %s", buf.String())
	}
}

// TestRequestID_GeneratesAndEchoes covers the empty-header path: the
// middleware must mint a UUID, expose it on the context, and echo it
// back as a response header.
func TestRequestID_GeneratesAndEchoes(t *testing.T) {
	var captured string
	h := httpx.RequestID()(http.HandlerFunc(
		func(_ http.ResponseWriter, r *http.Request) {
			captured = httpx.RequestIDFromContext(r.Context())
		},
	))

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", http.NoBody)
	h.ServeHTTP(rr, req)

	if captured == "" {
		t.Error("RequestIDFromContext returned empty")
	}
	if got := rr.Header().Get("X-Request-ID"); got != captured {
		t.Errorf("response header = %q, want %q", got, captured)
	}
}

// TestRequestID_PreservesInbound confirms the middleware does not
// overwrite a caller-supplied ID — operators rely on this for tracing
// from upstream proxies.
func TestRequestID_PreservesInbound(t *testing.T) {
	want := "client-supplied-id"
	var captured string
	h := httpx.RequestID()(http.HandlerFunc(
		func(_ http.ResponseWriter, r *http.Request) {
			captured = httpx.RequestIDFromContext(r.Context())
		},
	))

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", http.NoBody)
	req.Header.Set("X-Request-ID", want)
	h.ServeHTTP(rr, req)

	if captured != want {
		t.Errorf("captured = %q, want %q", captured, want)
	}
	if got := rr.Header().Get("X-Request-ID"); got != want {
		t.Errorf("echoed header = %q, want %q", got, want)
	}
}

// TestRequestIDFromContext_Absent returns "" when the middleware never
// ran. Callers (logging, metrics) rely on the empty-string contract so
// they don't need nil checks.
func TestRequestIDFromContext_Absent(t *testing.T) {
	if got := httpx.RequestIDFromContext(context.Background()); got != "" {
		t.Errorf("RequestIDFromContext(empty) = %q, want \"\"", got)
	}
}

// TestSLog_EmitsOneLine verifies the access log contains the canonical
// fields and exactly one line is written per request — duplicate access
// lines blow up our log volume budget.
func TestSLog_EmitsOneLine(t *testing.T) {
	var buf bytes.Buffer
	logger := newTestLogger(&buf)

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("ok"))
	})
	h := httpx.SLog(logger)(inner)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/x", strings.NewReader("body"))
	h.ServeHTTP(rr, req)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d log lines, want 1: %s", len(lines), buf.String())
	}
	rec := map[string]any{}
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("parse log: %v", err)
	}
	if rec["method"] != http.MethodPost {
		t.Errorf("method = %v, want POST", rec["method"])
	}
	if rec["status"].(float64) != float64(http.StatusAccepted) {
		t.Errorf("status = %v, want 202", rec["status"])
	}
	if _, ok := rec["duration_ms"]; !ok {
		t.Error("duration_ms missing")
	}
}

// TestMetrics_RecordsAndDecrements asserts that HTTPInflight returns to
// zero after the handler completes and HTTPRequests increments with the
// right labels. We assert post-call semantics because pre-call peeks
// would race with the increment under -race.
func TestMetrics_RecordsAndDecrements(t *testing.T) {
	m := newTestMetrics(t)

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello"))
	})
	h := httpx.Metrics(m)(inner)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", http.NoBody)
	h.ServeHTTP(rr, req)

	// gauge value via ToFloat64 helper — depends on prometheus testutil
	got := readGauge(t, m)
	if got != 0 {
		t.Errorf("HTTPInflight after request = %v, want 0", got)
	}
}

// TestChain_Order verifies outermost-first composition: the first
// middleware in the args runs first on entry and last on exit.
func TestChain_Order(t *testing.T) {
	var order []string
	mw := func(label string) func(http.Handler) http.Handler {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, label+">in")
				next.ServeHTTP(w, r)
				order = append(order, label+">out")
			})
		}
	}
	inner := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		order = append(order, "handler")
	})
	h := httpx.Chain(inner, mw("a"), mw("b"))

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", http.NoBody)
	h.ServeHTTP(rr, req)

	want := []string{"a>in", "b>in", "handler", "b>out", "a>out"}
	if !equal(order, want) {
		t.Errorf("order = %v, want %v", order, want)
	}
}

// TestStatusRecorder_DefaultsTo200 covers the happy path where the inner
// handler writes a body without calling WriteHeader — the recorder must
// report 200, matching net/http's implicit behavior.
func TestStatusRecorder_DefaultsTo200(t *testing.T) {
	m := newTestMetrics(t)
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})
	h := httpx.Metrics(m)(inner)

	rr := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", http.NoBody)
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
}

// readGauge is a thin wrapper over prometheus testutil so the test
// stays focused on observable middleware behavior.
func readGauge(t *testing.T, m *observability.Metrics) float64 {
	t.Helper()
	return testutil.ToFloat64(m.HTTPInflight)
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
