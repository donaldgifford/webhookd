// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

package webhook_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/donaldgifford/webhookd/internal/config"
	"github.com/donaldgifford/webhookd/internal/observability"
	"github.com/donaldgifford/webhookd/internal/webhook"
)

// fixedNow returns a stable clock so signatures and timestamps line up
// across the test table.
var fixedNow = time.Unix(1_700_000_000, 0)

func sign(secret []byte, ts string, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("v0:" + ts + ":"))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

type fixture struct {
	t   *testing.T
	cfg webhook.HandlerConfig
	m   *observability.Metrics
	h   http.Handler
	buf *bytes.Buffer
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	var buf bytes.Buffer
	logger := observability.NewLogger(&buf, slog.LevelDebug, "json")
	_, m := observability.NewMetrics(&config.Config{})
	cfg := webhook.HandlerConfig{
		SigningSecret:   []byte("topsecret"),
		MaxBodyBytes:    1 << 16,
		SignatureHeader: "X-Webhook-Signature",
		TimestampHeader: "X-Webhook-Timestamp",
		TimestampSkew:   5 * time.Minute,
		Now:             func() time.Time { return fixedNow },
	}
	return &fixture{
		t:   t,
		cfg: cfg,
		m:   m,
		h:   webhook.NewHandler(cfg, logger, m),
		buf: &buf,
	}
}

// doRequest issues a fixture-bound POST to /webhook/<provider>. Tests
// exercise different providers to confirm the path-value plumbing wires
// the right metric labels.
func (f *fixture) doRequest(provider string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	f.t.Helper()
	mux := http.NewServeMux()
	mux.Handle("POST /webhook/{provider}", f.h)

	req := httptest.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		"/webhook/"+provider,
		io.NopCloser(bytes.NewReader(body)),
	)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	return rr
}

func TestHandler_HappyPath(t *testing.T) {
	f := newFixture(t)
	body := []byte(`{"event_type":"push","data":{"x":1}}`)
	ts := strconv.FormatInt(fixedNow.Unix(), 10)
	rr := f.doRequest("github", body, map[string]string{
		f.cfg.SignatureHeader: sign(f.cfg.SigningSecret, ts, body),
		f.cfg.TimestampHeader: ts,
	})

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202\nlog: %s", rr.Code, f.buf.String())
	}
	if got := testutil.ToFloat64(
		f.m.WebhookEvents.WithLabelValues("github", "push", "accepted"),
	); got != 1 {
		t.Errorf("accepted counter = %v, want 1", got)
	}
	if got := testutil.ToFloat64(
		f.m.WebhookSigResults.WithLabelValues("github", "valid"),
	); got != 1 {
		t.Errorf("valid sig counter = %v, want 1", got)
	}
	if !strings.Contains(f.buf.String(), "webhook accepted") {
		t.Errorf("expected accept log, got: %s", f.buf.String())
	}
}

func TestHandler_InvalidSignature(t *testing.T) {
	f := newFixture(t)
	body := []byte(`{"event_type":"push"}`)
	ts := strconv.FormatInt(fixedNow.Unix(), 10)
	rr := f.doRequest("github", body, map[string]string{
		f.cfg.SignatureHeader: "sha256=" + strings.Repeat("aa", 32),
		f.cfg.TimestampHeader: ts,
	})

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if got := testutil.ToFloat64(
		f.m.WebhookSigResults.WithLabelValues("github", "invalid"),
	); got != 1 {
		t.Errorf("invalid sig counter = %v, want 1", got)
	}
	if got := testutil.ToFloat64(
		f.m.WebhookEvents.WithLabelValues("github", "", "invalid_signature"),
	); got != 1 {
		t.Errorf("invalid_signature outcome = %v, want 1", got)
	}
}

func TestHandler_MissingTimestamp(t *testing.T) {
	f := newFixture(t)
	body := []byte(`{"event_type":"push"}`)
	rr := f.doRequest("github", body, map[string]string{
		f.cfg.SignatureHeader: sign(f.cfg.SigningSecret, "0", body),
	})

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if got := testutil.ToFloat64(
		f.m.WebhookSigResults.WithLabelValues("github", "missing"),
	); got != 1 {
		t.Errorf("missing sig counter = %v, want 1", got)
	}
}

func TestHandler_BodyTooLarge(t *testing.T) {
	f := newFixture(t)
	f.cfg.MaxBodyBytes = 16
	// Rebuild with the smaller cap.
	logger := observability.NewLogger(f.buf, slog.LevelDebug, "json")
	f.h = webhook.NewHandler(f.cfg, logger, f.m)

	body := bytes.Repeat([]byte("x"), 1024)
	ts := strconv.FormatInt(fixedNow.Unix(), 10)
	rr := f.doRequest("github", body, map[string]string{
		f.cfg.SignatureHeader: sign(f.cfg.SigningSecret, ts, body),
		f.cfg.TimestampHeader: ts,
	})

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rr.Code)
	}
	if got := testutil.ToFloat64(
		f.m.WebhookEvents.WithLabelValues("github", "", "too_large"),
	); got != 1 {
		t.Errorf("too_large counter = %v, want 1", got)
	}
}

func TestHandler_MalformedJSON(t *testing.T) {
	f := newFixture(t)
	body := []byte(`not json`)
	ts := strconv.FormatInt(fixedNow.Unix(), 10)
	rr := f.doRequest("github", body, map[string]string{
		f.cfg.SignatureHeader: sign(f.cfg.SigningSecret, ts, body),
		f.cfg.TimestampHeader: ts,
	})

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rr.Code)
	}
	if got := testutil.ToFloat64(
		f.m.WebhookEvents.WithLabelValues("github", "", "malformed"),
	); got != 1 {
		t.Errorf("malformed counter = %v, want 1", got)
	}
}

func TestHandler_TimestampSkewed(t *testing.T) {
	f := newFixture(t)
	body := []byte(`{"event_type":"push"}`)
	stale := strconv.FormatInt(fixedNow.Add(-time.Hour).Unix(), 10)
	// Different provider here so the path-value plumbing into metric
	// labels is exercised on more than one value.
	rr := f.doRequest("discord", body, map[string]string{
		f.cfg.SignatureHeader: sign(f.cfg.SigningSecret, stale, body),
		f.cfg.TimestampHeader: stale,
	})

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
	if got := testutil.ToFloat64(
		f.m.WebhookEvents.WithLabelValues("discord", "", "timestamp_skewed"),
	); got != 1 {
		t.Errorf("skew outcome counter = %v, want 1", got)
	}
}
