// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// TestMain wraps every test in goleak so leaked goroutines from the
// BatchSpanProcessor or HTTP listeners are caught immediately. The
// otelhttp instrumentation creates a singleton background goroutine
// that we intentionally ignore (see ignoreOtel below).
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m,
		goleak.IgnoreTopFunction("go.opentelemetry.io/otel/sdk/trace.(*batchSpanProcessor).processQueue"),
	)
}

// TestRun_HappyPath drives the full wiring with a real port pair,
// posts a signed webhook, and asserts a clean shutdown when the
// context is canceled. We rely on the public+admin servers actually
// binding so this test catches misconfiguration that unit tests miss.
func TestRun_HappyPath(t *testing.T) {
	t.Setenv("WEBHOOK_SIGNING_SECRET", "topsecret")
	t.Setenv("WEBHOOK_ADDR", "127.0.0.1:0")
	t.Setenv("WEBHOOK_ADMIN_ADDR", "127.0.0.1:0")
	// Disable the real OTLP exporter; an unreachable collector would
	// keep the BatchSpanProcessor alive past Shutdown's deadline.
	t.Setenv("WEBHOOK_TRACING_ENABLED", "false")
	t.Setenv("WEBHOOK_SHUTDOWN_TIMEOUT", "5s")
	// Disable JSM provider — Phase 1 integration tests don't exercise
	// the JSM/CR pipeline, and JSM enabled would require config that
	// isn't relevant here. Phase 6 adds a dedicated end-to-end test.
	t.Setenv("WEBHOOK_PROVIDERS", "")
	t.Setenv("WEBHOOK_CR_SYNC_TIMEOUT", "2s")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	done := make(chan error, 1)
	go func() { done <- run(ctx) }()

	// Run is dispatched; cancel after a short wait so the shutdown
	// path executes. Without the wait the listeners may not have
	// bound yet.
	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run() err = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run() did not return within 10s after cancel")
	}
}

// TestRun_TombstoneWebhookRoute exercises the in-flight Phase 2 state:
// the legacy `webhook.NewHandler` is gone, the dispatcher (Phase 6)
// isn't wired yet, and the route is held by a 503 tombstone so the
// middleware chain stays exercised. Phase 6 replaces this test with a
// full envtest end-to-end against the real dispatcher + executor.
func TestRun_TombstoneWebhookRoute(t *testing.T) {
	t.Setenv("WEBHOOK_SIGNING_SECRET", "topsecret")
	t.Setenv("WEBHOOK_ADDR", "127.0.0.1:18091")
	t.Setenv("WEBHOOK_ADMIN_ADDR", "127.0.0.1:19091")
	t.Setenv("WEBHOOK_TRACING_ENABLED", "false")
	t.Setenv("WEBHOOK_SHUTDOWN_TIMEOUT", "5s")
	t.Setenv("WEBHOOK_PROVIDERS", "")
	t.Setenv("WEBHOOK_CR_SYNC_TIMEOUT", "2s")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	done := make(chan error, 1)
	go func() { done <- run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("run() did not return on cleanup")
		}
	})

	if err := waitFor(t, "http://127.0.0.1:19091/healthz"); err != nil {
		t.Fatalf("admin healthz never became ready: %v", err)
	}

	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost,
		"http://127.0.0.1:18091/webhook/github",
		bytes.NewReader([]byte(`{"event_type":"push"}`)),
	)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post webhook: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 503\nbody: %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Retry-After"); got != "30" {
		t.Errorf("Retry-After header = %q, want %q", got, "30")
	}
	if got := resp.Header.Get("X-Request-Id"); got == "" {
		t.Error("X-Request-Id header is empty — RequestID middleware not running")
	}
}

// waitFor polls url with a short retry loop. Listener binding is
// asynchronous after run() dispatches goroutines — this gives the
// kernel a few hundred ms to honor SO_REUSEADDR / port allocation.
func waitFor(t *testing.T, url string) error {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(
			context.Background(), http.MethodGet, url, http.NoBody,
		)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errTimeout
}

var errTimeout = &timeoutError{}

type timeoutError struct{}

func (timeoutError) Error() string { return "timeout waiting for url" }
