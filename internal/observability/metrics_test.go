package observability_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/donaldgifford/webhookd/internal/config"
	"github.com/donaldgifford/webhookd/internal/observability"
)

// TestNewMetrics_ExposesAllInstruments scrapes the registry and asserts
// that every instrument from DESIGN-0001 §Metrics, plus the runtime
// collectors and build_info, appears in the exposition output. We force
// at least one observation on each vec so the metric appears in the
// scrape — vec metrics with no labeled children render no lines.
func TestNewMetrics_ExposesAllInstruments(t *testing.T) {
	cfg := &config.Config{
		BuildInfo: config.BuildInfo{
			Version:   "v1.2.3",
			Commit:    "abc123",
			GoVersion: "go1.26.1",
		},
	}
	reg, m := observability.NewMetrics(cfg)

	// Touch every vec so each renders at least one line.
	m.HTTPRequests.WithLabelValues("GET", "/healthz", "200").Inc()
	m.HTTPDuration.WithLabelValues("GET", "/healthz", "200").Observe(0.01)
	m.HTTPRequestSize.WithLabelValues("GET", "/healthz").Observe(0)
	m.HTTPResponseSize.WithLabelValues("GET", "/healthz").Observe(0)
	m.HTTPInflight.Set(0)
	m.HTTPPanics.Add(0)
	m.WebhookEvents.WithLabelValues("github", "push", "accepted").Inc()
	m.WebhookSigResults.WithLabelValues("github", "valid").Inc()
	m.WebhookProcessing.WithLabelValues("github", "push").Observe(0.05)

	body := scrape(t, reg)

	want := []string{
		"webhookd_http_requests_total",
		"webhookd_http_request_duration_seconds",
		"webhookd_http_request_size_bytes",
		"webhookd_http_response_size_bytes",
		"webhookd_http_inflight_requests",
		"webhookd_http_panics_total",
		"webhookd_webhook_events_total",
		"webhookd_webhook_signature_validation_total",
		"webhookd_webhook_processing_duration_seconds",
		"webhookd_build_info",
		"go_goroutines",
		"process_cpu_seconds_total",
	}
	for _, name := range want {
		if !strings.Contains(body, name) {
			t.Errorf("metric %q missing from scrape", name)
		}
	}
}

// TestNewMetrics_BuildInfoLabels confirms the build_info metric carries
// the BuildInfo values and is fixed at 1.
func TestNewMetrics_BuildInfoLabels(t *testing.T) {
	cfg := &config.Config{
		BuildInfo: config.BuildInfo{
			Version:   "v9.9.9",
			Commit:    "deadbeef",
			GoVersion: "go1.26.1",
		},
	}
	reg, _ := observability.NewMetrics(cfg)
	body := scrape(t, reg)

	want := `webhookd_build_info{commit="deadbeef",go_version="go1.26.1",version="v9.9.9"} 1`
	if !strings.Contains(body, want) {
		t.Errorf("build_info line not found in scrape\nwant: %s\ngot:\n%s",
			want, body)
	}
}

// TestNewMetrics_FreshRegistry ensures repeated calls do not collide on
// the default registerer — a critical property for parallel tests.
func TestNewMetrics_FreshRegistry(t *testing.T) {
	cfg := &config.Config{BuildInfo: config.BuildInfo{Version: "v1"}}
	reg1, _ := observability.NewMetrics(cfg)
	reg2, _ := observability.NewMetrics(cfg)
	if reg1 == reg2 {
		t.Errorf("NewMetrics returned same registry on two calls")
	}
}

// scrape exercises the metrics registry through the same handler that
// would be mounted on /metrics in production, so we verify exposition
// format end-to-end rather than peeking at internal state.
func scrape(t *testing.T, reg prometheus.Gatherer) string {
	t.Helper()
	srv := httptest.NewServer(promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	defer srv.Close()

	req, err := http.NewRequestWithContext(
		context.Background(), http.MethodGet, srv.URL, http.NoBody,
	)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(body)
}
