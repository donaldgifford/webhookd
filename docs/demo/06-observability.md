# 06. Observability

Three signal pipelines, kept independent:

- **Logs** — stdlib `log/slog` JSON handler with trace-correlation
- **Metrics** — Prometheus, **private registry** so tests can spin up
  isolated harnesses
- **Traces** — OpenTelemetry SDK with OTLP/gRPC exporter

The split mirrors the production webhookd (ADR-0002): metrics on
Prometheus, traces on OTel, logs on slog. No bridge layer.

## Files in this phase

```
internal/observability/
├── logging.go        # slog JSON handler with trace correlation
├── metrics.go        # private registry + handler-level metrics
└── tracing.go        # OTel TracerProvider + OTLP exporter
```

## Logging

`slog.Handler` that:

- Writes JSON to stderr by default (configurable to stdout/file)
- Attaches `trace_id` and `span_id` from the active span when present
- Honors a configurable level (`debug` / `info` / `warn` / `error`)

### `internal/observability/logging.go`

```go
// Package observability builds the demo's logging, metrics, and tracing
// primitives. Each function returns a fresh, isolated artifact —
// callers wire them into the dispatcher and HTTP framework.
package observability

import (
    "context"
    "io"
    "log/slog"
    "strings"

    "go.opentelemetry.io/otel/trace"
)

// LoggerOption tunes NewLogger.
type LoggerOption func(*loggerOpts)

type loggerOpts struct {
    level  slog.Level
    out    io.Writer
    source bool
}

// WithLevel sets the slog level. Accepts "debug", "info", "warn", "error".
func WithLevel(level string) LoggerOption {
    return func(o *loggerOpts) {
        switch strings.ToLower(level) {
        case "debug":
            o.level = slog.LevelDebug
        case "warn":
            o.level = slog.LevelWarn
        case "error":
            o.level = slog.LevelError
        default:
            o.level = slog.LevelInfo
        }
    }
}

// WithOutput sets where the logger writes (default os.Stderr).
func WithOutput(w io.Writer) LoggerOption {
    return func(o *loggerOpts) { o.out = w }
}

// WithSource toggles slog's AddSource (file:line) attribute.
func WithSource(on bool) LoggerOption {
    return func(o *loggerOpts) { o.source = on }
}

// NewLogger returns a *slog.Logger that emits JSON and decorates each
// record with trace_id/span_id when an active OTel span is present.
func NewLogger(opts ...LoggerOption) *slog.Logger {
    o := loggerOpts{level: slog.LevelInfo}
    for _, opt := range opts {
        opt(&o)
    }
    if o.out == nil {
        o.out = stderr()
    }

    base := slog.NewJSONHandler(o.out, &slog.HandlerOptions{
        Level:     o.level,
        AddSource: o.source,
    })
    return slog.New(&traceHandler{base: base})
}

// traceHandler decorates every record with trace_id/span_id when the
// record's context has an active OTel span.
type traceHandler struct {
    base slog.Handler
}

func (h *traceHandler) Enabled(ctx context.Context, l slog.Level) bool {
    return h.base.Enabled(ctx, l)
}

func (h *traceHandler) Handle(ctx context.Context, r slog.Record) error {
    if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
        r.AddAttrs(
            slog.String("trace_id", sc.TraceID().String()),
            slog.String("span_id", sc.SpanID().String()),
        )
    }
    return h.base.Handle(ctx, r)
}

func (h *traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
    return &traceHandler{base: h.base.WithAttrs(attrs)}
}

func (h *traceHandler) WithGroup(name string) slog.Handler {
    return &traceHandler{base: h.base.WithGroup(name)}
}

// stderr is split out so tests can stub it.
var stderr = func() io.Writer {
    return realStderr
}
```

The tiny `stderr` indirection lets a test substitute a `bytes.Buffer`
without globally redirecting `os.Stderr`. We need to import `os` once
and assign:

```go
// internal/observability/logging_stderr.go
package observability

import "os"

var realStderr = os.Stderr
```

## Metrics

A private `*prometheus.Registry` (no `DefaultRegisterer`) so each test
gets fresh state and the production binary can deterministically scrape
exactly the metrics we register.

### `internal/observability/metrics.go`

```go
package observability

import (
    "github.com/prometheus/client_golang/prometheus"
)

// Metrics is the bundle of every Prometheus collector the demo registers.
// Pass it through to consumers (dispatcher, backends) so they can record
// observations without reaching for global state.
type Metrics struct {
    Registry *prometheus.Registry

    // HTTP layer.
    HTTPRequestsTotal      *prometheus.CounterVec
    HTTPRequestDuration    *prometheus.HistogramVec
    HTTPInFlight           prometheus.Gauge
    SignatureFailures      *prometheus.CounterVec
    RateLimitDrops         *prometheus.CounterVec

    // Dispatcher layer.
    DispatchTotal          *prometheus.CounterVec
    DispatchDuration       *prometheus.HistogramVec
    IdempotencyHits        *prometheus.CounterVec

    // Backend layer.
    BackendApplyTotal      *prometheus.CounterVec
    BackendSyncDuration    *prometheus.HistogramVec
}

// NewMetrics builds a fresh Metrics with all collectors registered to a
// private registry. Returns the Metrics struct and the registry (so the
// caller can mount /metrics).
func NewMetrics(buildVersion string) *Metrics {
    reg := prometheus.NewRegistry()
    factory := prometheus.WrapRegistererWith(prometheus.Labels{
        "version": buildVersion,
    }, reg)

    m := &Metrics{
        Registry: reg,

        HTTPRequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
            Name: "webhookd_http_requests_total",
            Help: "Total HTTP requests by provider, method, and status.",
        }, []string{"provider", "method", "status"}),

        HTTPRequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
            Name:    "webhookd_http_request_duration_seconds",
            Help:    "HTTP request duration in seconds.",
            Buckets: prometheus.DefBuckets,
        }, []string{"provider", "method"}),

        HTTPInFlight: prometheus.NewGauge(prometheus.GaugeOpts{
            Name: "webhookd_http_in_flight_requests",
            Help: "Number of in-flight HTTP requests.",
        }),

        SignatureFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
            Name: "webhookd_signature_failures_total",
            Help: "Total signature verification failures by provider.",
        }, []string{"provider"}),

        RateLimitDrops: prometheus.NewCounterVec(prometheus.CounterOpts{
            Name: "webhookd_rate_limit_drops_total",
            Help: "Total requests dropped by the rate limiter, by provider.",
        }, []string{"provider"}),

        DispatchTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
            Name: "webhookd_dispatch_total",
            Help: "Dispatch outcomes by instance and result kind.",
        }, []string{"instance", "kind"}),

        DispatchDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
            Name:    "webhookd_dispatch_duration_seconds",
            Help:    "Dispatch duration in seconds.",
            Buckets: prometheus.DefBuckets,
        }, []string{"instance"}),

        IdempotencyHits: prometheus.NewCounterVec(prometheus.CounterOpts{
            Name: "webhookd_idempotency_hits_total",
            Help: "Idempotency cache hits (deduped requests).",
        }, []string{"instance"}),

        BackendApplyTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
            Name: "webhookd_backend_apply_total",
            Help: "Backend apply outcomes by backend type and result.",
        }, []string{"backend", "outcome"}),

        BackendSyncDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
            Name:    "webhookd_backend_sync_duration_seconds",
            Help:    "Backend synchronous-wait duration in seconds.",
            Buckets: prometheus.DefBuckets,
        }, []string{"backend", "outcome"}),
    }

    factory.MustRegister(
        m.HTTPRequestsTotal,
        m.HTTPRequestDuration,
        m.HTTPInFlight,
        m.SignatureFailures,
        m.RateLimitDrops,
        m.DispatchTotal,
        m.DispatchDuration,
        m.IdempotencyHits,
        m.BackendApplyTotal,
        m.BackendSyncDuration,
    )

    // Register the standard Go runtime + process collectors.
    reg.MustRegister(prometheus.NewGoCollector())
    reg.MustRegister(prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))

    return m
}
```

## Tracing

OTel SDK + OTLP/gRPC exporter. The exporter target is `localhost:4317`
in the demo (matches `otel-collector.yaml`).

### `internal/observability/tracing.go`

```go
package observability

import (
    "context"
    "fmt"

    "go.opentelemetry.io/otel"
    "go.opentelemetry.io/otel/exporters/otlp/otlptrace"
    "go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
    "go.opentelemetry.io/otel/propagation"
    "go.opentelemetry.io/otel/sdk/resource"
    sdktrace "go.opentelemetry.io/otel/sdk/trace"
    semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// TracerProvider is what callers receive. Wraps the OTel TracerProvider
// with a Shutdown() that's safe to defer.
type TracerProvider struct {
    tp *sdktrace.TracerProvider
}

// Shutdown flushes pending spans and closes the exporter. Safe to call
// with a context that's already cancelled — pending spans are
// best-effort dropped.
func (p *TracerProvider) Shutdown(ctx context.Context) error {
    if p == nil || p.tp == nil {
        return nil
    }
    return p.tp.Shutdown(ctx)
}

// TracingOptions matches the HCL TracingBlock fields.
type TracingOptions struct {
    Enabled  bool
    Endpoint string
    Service  string
    Version  string
}

// NewTracerProvider builds a TracerProvider exporting OTLP/gRPC to the
// configured endpoint. When opts.Enabled is false, a no-op provider is
// returned and tracing is disabled globally.
func NewTracerProvider(ctx context.Context, opts TracingOptions) (*TracerProvider, error) {
    if !opts.Enabled {
        // No-op provider — but still install a propagator so headers
        // round-trip cleanly even with tracing off.
        otel.SetTextMapPropagator(propagation.TraceContext{})
        return &TracerProvider{}, nil
    }
    if opts.Endpoint == "" {
        return nil, fmt.Errorf("tracing enabled but endpoint is empty")
    }

    exp, err := otlptrace.New(ctx, otlptracegrpc.NewClient(
        otlptracegrpc.WithEndpoint(opts.Endpoint),
        otlptracegrpc.WithInsecure(),
    ))
    if err != nil {
        return nil, fmt.Errorf("build otlp exporter: %w", err)
    }

    res, err := resource.Merge(
        resource.Default(),
        resource.NewWithAttributes(
            semconv.SchemaURL,
            semconv.ServiceName(opts.Service),
            semconv.ServiceVersion(opts.Version),
        ),
    )
    if err != nil {
        return nil, fmt.Errorf("build resource: %w", err)
    }

    tp := sdktrace.NewTracerProvider(
        sdktrace.WithBatcher(exp),
        sdktrace.WithResource(res),
        sdktrace.WithSampler(sdktrace.AlwaysSample()),
    )
    otel.SetTracerProvider(tp)
    otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
        propagation.TraceContext{},
        propagation.Baggage{},
    ))

    return &TracerProvider{tp: tp}, nil
}
```

> `WithInsecure()` is technically deprecated in favor of
> `WithDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))`
> from `google.golang.org/grpc` + `google.golang.org/grpc/credentials/insecure`.
> The simpler form is fine for a demo against a local OTel collector;
> production code should use the explicit-credentials form.

## Trace correlation in slog

The `traceHandler` in `logging.go` reads the active span from the
record's context and stamps `trace_id` + `span_id` on every entry.

In practice that means anywhere you log via the request context, you
get correlation for free:

```go
// In a handler:
ctx, span := tracer.Start(r.Context(), "dispatcher.handle")
defer span.End()

slog.InfoContext(ctx, "processing", "instance", inst.ID)
// → {"level":"INFO","msg":"processing","instance":"demo-tenant-a","trace_id":"...","span_id":"..."}
```

Without `slog.InfoContext`, no trace ID. Use the context-aware methods.

## Wiring preview

`main.go` will use these like this (full file in phase 9):

```go
// shutdown order matters:
// 1) HTTP server (stop accepting new requests)
// 2) tracer provider (flush pending spans)
// 3) logger (no explicit shutdown)
log := observability.NewLogger(observability.WithLevel(logLevel))

m := observability.NewMetrics(buildVersion)
tp, err := observability.NewTracerProvider(ctx, observability.TracingOptions{
    Enabled:  cfg.Runtime.Tracing != nil && cfg.Runtime.Tracing.Enabled,
    Endpoint: cfg.Runtime.Tracing.Endpoint,
    Service:  cfg.Runtime.Tracing.Service,
    Version:  buildVersion,
})
if err != nil {
    log.Error("init tracing", "err", err)
    return 1
}
defer tp.Shutdown(context.Background())
```

## What we proved

- [x] slog handler decorates every `*Context` log call with trace_id/span_id
- [x] Metrics live on a private registry — no `DefaultRegisterer` leakage
- [x] OTel tracer provider has a clean `Shutdown` for graceful drain
- [x] All three pipelines are independent — tests can use just one

Next: [07-http.md](07-http.md) — the HTTP framework on top of these.
