package httpx_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/donaldgifford/webhookd/internal/config"
	"github.com/donaldgifford/webhookd/internal/httpx"
	"github.com/donaldgifford/webhookd/internal/observability"
)

// TestAdmin_HealthzAlways200 covers the liveness contract: /healthz
// returns 200 even before readiness flips. Kubernetes uses this to
// decide whether to restart the pod; flapping to non-200 would cause
// unwanted restarts.
func TestAdmin_HealthzAlways200(t *testing.T) {
	srv := newAdminServer(t, false)
	t.Cleanup(srv.Close)

	body, status := getURL(t, srv.URL+"/healthz")
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	if !strings.Contains(body, "ok") {
		t.Errorf("body = %q, want ok", body)
	}
}

// TestAdmin_Readyz_FollowsAtomicBool is the contract we wire main against
// — the value can flip during a drain to take a replica out of rotation
// without restarting it.
func TestAdmin_Readyz_FollowsAtomicBool(t *testing.T) {
	var ready atomic.Bool
	srv := newAdminServerWith(t, &ready)
	t.Cleanup(srv.Close)

	_, status := getURL(t, srv.URL+"/readyz")
	if status != http.StatusServiceUnavailable {
		t.Errorf("not-ready status = %d, want 503", status)
	}

	ready.Store(true)
	_, status = getURL(t, srv.URL+"/readyz")
	if status != http.StatusOK {
		t.Errorf("ready status = %d, want 200", status)
	}

	ready.Store(false)
	_, status = getURL(t, srv.URL+"/readyz")
	if status != http.StatusServiceUnavailable {
		t.Errorf("re-drained status = %d, want 503", status)
	}
}

// TestAdmin_MetricsExposes confirms the registry handed to the admin mux
// is what gets scraped — a regression here would hide every webhookd
// metric from Prometheus despite passing unit tests on the registry
// alone.
func TestAdmin_MetricsExposes(t *testing.T) {
	srv := newAdminServer(t, true)
	t.Cleanup(srv.Close)

	// Hit /healthz first so the Metrics middleware records a child on
	// HTTPRequests; without that, the vec renders no lines and we can't
	// observe end-to-end registry mounting.
	if _, status := getURL(t, srv.URL+"/healthz"); status != http.StatusOK {
		t.Fatalf("healthz status = %d", status)
	}

	body, status := getURL(t, srv.URL+"/metrics")
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	wantSubs := []string{
		"webhookd_http_requests_total",
		"webhookd_http_inflight_requests",
		"webhookd_build_info",
	}
	for _, s := range wantSubs {
		if !strings.Contains(body, s) {
			t.Errorf("metrics body missing %q", s)
		}
	}
}

func newAdminServer(t *testing.T, ready bool) *httptest.Server {
	t.Helper()
	var b atomic.Bool
	b.Store(ready)
	return newAdminServerWith(t, &b)
}

func newAdminServerWith(t *testing.T, ready *atomic.Bool) *httptest.Server {
	t.Helper()
	var buf bytes.Buffer
	logger := newTestLogger(&buf)
	reg, m := observability.NewMetrics(&config.Config{})
	return httptest.NewServer(httpx.NewAdminMux(logger, reg, m, ready))
}

func getURL(t *testing.T, url string) (string, int) {
	t.Helper()
	req, err := http.NewRequestWithContext(
		context.Background(), http.MethodGet, url, http.NoBody,
	)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(body), resp.StatusCode
}
