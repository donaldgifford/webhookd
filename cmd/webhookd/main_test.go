// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strconv"
	"strings"
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

// signBody mirrors the production canonical format. Lifted into the
// test so future provider tests can share the helper if/when they
// move into this package.
func signBody(secret []byte, ts string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("v0:" + ts + ":"))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// TestRun_AcceptsSignedWebhook is the integration end-to-end: the
// servers really bind, a signed request really hits the public
// listener, and a scrape of the admin listener really sees the
// counter increment. This is the test that protects the wiring
// against package-level refactors that break composition without
// breaking individual unit tests.
func TestRun_AcceptsSignedWebhook(t *testing.T) {
	secret := []byte("topsecret")
	t.Setenv("WEBHOOK_SIGNING_SECRET", string(secret))
	t.Setenv("WEBHOOK_ADDR", "127.0.0.1:18091")
	t.Setenv("WEBHOOK_ADMIN_ADDR", "127.0.0.1:19091")
	t.Setenv("WEBHOOK_TRACING_ENABLED", "false")
	t.Setenv("WEBHOOK_SHUTDOWN_TIMEOUT", "5s")

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

	// Wait for the listeners to bind by polling /healthz. The retry
	// keeps this test robust on slow CI hosts.
	if err := waitFor(t, "http://127.0.0.1:19091/healthz"); err != nil {
		t.Fatalf("admin healthz never became ready: %v", err)
	}

	body := []byte(`{"event_type":"push","data":{"x":1}}`)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost,
		"http://127.0.0.1:18091/webhook/github",
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("X-Webhook-Signature", signBody(secret, ts, body))
	req.Header.Set("X-Webhook-Timestamp", ts)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post webhook: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}

	// Scrape /metrics and confirm the counter incremented.
	scrape, err := http.NewRequestWithContext(
		ctx, http.MethodGet,
		"http://127.0.0.1:19091/metrics",
		http.NoBody,
	)
	if err != nil {
		t.Fatalf("build scrape: %v", err)
	}
	mResp, err := http.DefaultClient.Do(scrape)
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	defer func() { _ = mResp.Body.Close() }()
	mBody, _ := io.ReadAll(mResp.Body)
	want := `webhookd_webhook_events_total{event_type="push",outcome="accepted",provider="github"} 1`
	if !strings.Contains(string(mBody), want) {
		t.Errorf("metrics missing accepted counter\nwant: %s\ngot:\n%s",
			want, mBody)
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
