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

	// Phase 2: K8s and JSM-specific instruments. The `_k8s_` prefix
	// (rather than `_cr_`) leaves room for future K8s ops outside the
	// CR-apply path (e.g., reference validation).

	// K8sApplyTotal counts every SSA Patch attempt by CR kind and
	// outcome — outcome ∈ {created, updated, unchanged, error}. The
	// "unchanged" outcome distinguishes a no-op SSA from a real apply,
	// which matters for dashboarding ticket churn.
	K8sApplyTotal *prometheus.CounterVec

	// K8sSyncDuration histograms the watch-loop latency from Patch
	// return to either Ready=True or timeout. Bucket boundaries
	// chosen so the JSM SLO (10s p95) lands between 10s and 20s.
	K8sSyncDuration *prometheus.HistogramVec

	// JSMPayloadParseErrors counts payload-rejected events by reason
	// (invalid_json | missing_field | wrong_type | empty_field).
	// Operators alert on a non-zero rate of `wrong_type` because
	// that's how a misconfigured JSM custom-field shows up.
	JSMPayloadParseErrors *prometheus.CounterVec

	// JSMNoopTotal counts NoopAction returns by the actual ticket
	// status that fired the webhook. Useful for spotting misconfigured
	// JSM automation rules ("why are we getting 500 noop's a day?").
	JSMNoopTotal *prometheus.CounterVec

	// JSMResponseTotal counts dispatcher responses by HTTP status
	// code. Phase 1's HTTPRequests counter already covers this for
	// HTTP transport telemetry; JSMResponseTotal lives at a
	// different label cardinality (no method/route, just the
	// JSM-relevant status) to keep dashboards focused.
	JSMResponseTotal *prometheus.CounterVec
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

// k8sSyncBuckets brackets the operator's reconcile budget. The 10s
// boundary aligns with the JSM SLO; below that is "fast", above is
// "slow but pre-timeout"; >30 only ever shows on timeout outcomes.
var k8sSyncBuckets = []float64{
	0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 30,
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
	addPhase2Metrics(m)

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
		m.K8sApplyTotal,
		m.K8sSyncDuration,
		m.JSMPayloadParseErrors,
		m.JSMNoopTotal,
		m.JSMResponseTotal,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		newBuildInfoCollector(cfg.BuildInfo),
	)
	return reg, m
}

// addPhase2Metrics fills the K8s + JSM-specific instruments on m.
// Pulled out of NewMetrics so that constructor stays under the funlen
// budget; the split lines up with Phase 1 (HTTP + webhook) vs Phase 2
// (K8s + JSM) responsibilities.
func addPhase2Metrics(m *Metrics) {
	m.K8sApplyTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "webhookd_k8s_apply_total",
			Help: "Server-Side Apply attempts by CR kind and outcome.",
		},
		[]string{"kind", "outcome"},
	)
	m.K8sSyncDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "webhookd_k8s_sync_duration_seconds",
			Help:    "Watch-loop latency from Patch return to Ready=True / timeout, by CR kind and outcome.",
			Buckets: k8sSyncBuckets,
		},
		[]string{"kind", "outcome"},
	)
	m.JSMPayloadParseErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "webhookd_jsm_payload_parse_errors_total",
			Help: "JSM payloads that failed to decode / extract, by reason.",
		},
		[]string{"reason"},
	)
	m.JSMNoopTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "webhookd_jsm_noop_total",
			Help: "JSM webhooks that returned NoopAction, by ticket status.",
		},
		[]string{"trigger_status"},
	)
	m.JSMResponseTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "webhookd_jsm_response_total",
			Help: "Dispatcher responses to JSM by HTTP status code.",
		},
		[]string{"status_code"},
	)
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
