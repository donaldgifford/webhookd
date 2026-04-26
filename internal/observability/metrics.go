// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 webhookd contributors

package observability

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/donaldgifford/webhookd/internal/config"
)

// Metrics holds every Prometheus instrument webhookd exports. Phase 1
// covers HTTP and webhook-domain instruments; rate-limit instruments
// arrive in Phase 6 and metric handles for new collectors land on this
// struct so callers do not have to thread additional plumbing.
//
// All instruments are registered on a private *prometheus.Registry — see
// NewMetrics — so test harnesses can spin up a fresh registry per run
// without leaking state through the default global registerer.
type Metrics struct {
	// HTTP-layer (recorded by httpx middleware in Phase 3).
	HTTPRequests     *prometheus.CounterVec
	HTTPDuration     *prometheus.HistogramVec
	HTTPRequestSize  *prometheus.HistogramVec
	HTTPResponseSize *prometheus.HistogramVec
	HTTPInflight     prometheus.Gauge
	HTTPPanics       prometheus.Counter

	// Webhook domain (recorded by the webhook handler in Phase 4).
	WebhookEvents     *prometheus.CounterVec
	WebhookSigResults *prometheus.CounterVec
	WebhookProcessing *prometheus.HistogramVec

	// Rate-limit (recorded by httpx.RateLimit middleware in Phase 6).
	HTTPRateLimited *prometheus.CounterVec
}

// httpDurationBuckets matches DESIGN-0001 §Metrics. The handler is a
// sub-second path; finer-grained low-end buckets give more useful
// percentile data than the Prometheus defaults.
var httpDurationBuckets = []float64{
	0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5,
}

// httpSizeBuckets covers typical webhook payload distributions: most
// providers post a few-KB JSON body, with occasional larger payloads from
// JSM full-issue events. Buckets stop at 1 MiB, the configured
// MaxBodyBytes default.
var httpSizeBuckets = []float64{
	256, 1024, 4096, 16384, 65536, 262144, 1048576,
}

// NewMetrics builds the registry, registers every instrument plus the
// runtime collectors, and returns both so the caller can mount the
// registry on the admin /metrics endpoint and pass *Metrics into
// middleware/handlers.
//
// The build label values come from cfg.BuildInfo, populated by main from
// -ldflags variables. Empty values are passed through unchanged so a dev
// build (commit="unknown") is still observable.
func NewMetrics(cfg *config.Config) (*prometheus.Registry, *Metrics) {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		HTTPRequests: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "webhookd_http_requests_total",
				Help: "Total HTTP requests by method, route, and status.",
			},
			[]string{"method", "route", "status"},
		),
		HTTPDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "webhookd_http_request_duration_seconds",
				Help:    "HTTP request latency by method, route, and status.",
				Buckets: httpDurationBuckets,
			},
			[]string{"method", "route", "status"},
		),
		HTTPRequestSize: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "webhookd_http_request_size_bytes",
				Help:    "HTTP request body size by method and route.",
				Buckets: httpSizeBuckets,
			},
			[]string{"method", "route"},
		),
		HTTPResponseSize: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "webhookd_http_response_size_bytes",
				Help:    "HTTP response body size by method and route.",
				Buckets: httpSizeBuckets,
			},
			[]string{"method", "route"},
		),
		HTTPInflight: prometheus.NewGauge(
			prometheus.GaugeOpts{
				Name: "webhookd_http_inflight_requests",
				Help: "In-flight HTTP requests across all routes.",
			},
		),
		HTTPPanics: prometheus.NewCounter(
			prometheus.CounterOpts{
				Name: "webhookd_http_panics_total",
				Help: "HTTP handlers that panicked and were recovered.",
			},
		),
		WebhookEvents: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "webhookd_webhook_events_total",
				Help: "Webhook events received by provider, event type, and outcome.",
			},
			[]string{"provider", "event_type", "outcome"},
		),
		WebhookSigResults: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "webhookd_webhook_signature_validation_total",
				Help: "Signature validation outcomes by provider and result.",
			},
			[]string{"provider", "result"},
		),
		WebhookProcessing: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "webhookd_webhook_processing_duration_seconds",
				Help:    "Webhook processing latency by provider and event type.",
				Buckets: httpDurationBuckets,
			},
			[]string{"provider", "event_type"},
		),
		HTTPRateLimited: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "webhookd_http_rate_limited_total",
				Help: "Requests rejected by the per-provider rate limiter.",
			},
			[]string{"provider"},
		),
	}

	reg.MustRegister(
		m.HTTPRequests,
		m.HTTPDuration,
		m.HTTPRequestSize,
		m.HTTPResponseSize,
		m.HTTPInflight,
		m.HTTPPanics,
		m.WebhookEvents,
		m.WebhookSigResults,
		m.WebhookProcessing,
		m.HTTPRateLimited,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		newBuildInfoCollector(cfg.BuildInfo),
	)
	return reg, m
}

// newBuildInfoCollector returns a constant gauge fixed at 1 with version,
// commit, and go_version exposed as labels. Dashboards use this metric
// to display deployed-build provenance and to alert on unexpected
// rollbacks.
func newBuildInfoCollector(b config.BuildInfo) prometheus.Collector {
	g := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "webhookd_build_info",
			Help: "Build provenance for the running webhookd process.",
		},
		[]string{"version", "commit", "go_version"},
	)
	g.WithLabelValues(b.Version, b.Commit, b.GoVersion).Set(1)
	return g
}
