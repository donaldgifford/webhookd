# webhookd — Phase 1 Walkthrough

A start-to-finish tour of the `webhookd` service: how the binary comes up, how a
request flows through it, where every metric is recorded, how every span is born
and closed, and — most importantly — how to extend any of it without breaking
the conventions.

Companion to: `docs/design/0001-stateless-webhook-receiver-phase-1.md`.

<!--toc:start-->
<!--toc:end-->

## 1. Source Tree

```
webhookd/
├── cmd/webhookd/main.go            # entry point, wiring only
└── internal/
    ├── config/config.go            # env-var parsing
    ├── observability/
    │   ├── logging.go              # slog + trace correlation
    │   ├── tracing.go              # OTel tracer provider
    │   └── metrics.go              # Prometheus registry + instruments
    ├── httpx/
    │   ├── middleware.go           # recover, request_id, slog, metrics
    │   ├── admin.go                # admin mux (metrics + probes)
    │   └── server.go               # *http.Server construction
    └── webhook/
        ├── signature.go            # HMAC verification
        └── handler.go              # the webhook handler
```

Two rules govern the layout:

1. `cmd/webhookd/main.go` is the only place where concrete types from different
   subpackages meet. Everywhere else depends on narrow interfaces or concrete
   types from a single sibling.
2. Every `internal/` subpackage has one reason to change. `metrics.go` changes
   when we add or retire a metric; `tracing.go` changes when we change exporter
   or sampling policy; the handler changes when business logic does.

## 2. Startup: `main.go` Line by Line

The entire startup path is about 80 lines of wiring. Read it as five phases:

```go
func main() {
    if err := run(context.Background()); err != nil {
        // We do not use slog here because slog may not be initialized
        // yet. Direct stderr write, then exit.
        fmt.Fprintf(os.Stderr, "startup: %v\n", err)
        os.Exit(1)
    }
}

func run(ctx context.Context) error {
    // Phase A — config
    cfg, err := config.Load()
    if err != nil {
        return fmt.Errorf("load config: %w", err)
    }

    // Phase B — observability
    logger := observability.NewLogger(cfg.LogLevel, cfg.LogFormat)
    slog.SetDefault(logger)

    tp, err := observability.NewTracerProvider(ctx, cfg)
    if err != nil {
        return fmt.Errorf("init tracer: %w", err)
    }
    defer func() {
        shutdownCtx, cancel := context.WithTimeout(
            context.Background(), cfg.ShutdownTimeout)
        defer cancel()
        if err := tp.Shutdown(shutdownCtx); err != nil {
            slog.Error("tracer shutdown", "err", err)
        }
    }()
    otel.SetTracerProvider(tp)
    otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
        propagation.TraceContext{}, propagation.Baggage{}))

    reg, metrics := observability.NewMetrics(cfg.BuildInfo)

    // Phase C — handlers and middleware
    readiness := &atomic.Bool{} // flipped true after listeners bind
    publicMux := http.NewServeMux()
    publicMux.Handle("POST /webhook/{provider}",
        webhook.NewHandler(cfg.SigningSecret, metrics))

    publicHandler := httpx.Chain(publicMux,
        httpx.Recover(metrics),
        httpx.OTel("webhookd"),       // otelhttp wrapper
        httpx.RequestID(),
        httpx.SLog(),
        httpx.Metrics(metrics),
    )

    adminHandler := httpx.NewAdminMux(reg, readiness)

    // Phase D — servers
    publicSrv := httpx.NewServer(cfg.Addr, publicHandler, cfg)
    adminSrv  := httpx.NewServer(cfg.AdminAddr, adminHandler, cfg)

    // Phase E — run + signal handling
    errCh := make(chan error, 2)
    go func() { errCh <- publicSrv.ListenAndServe() }()
    go func() { errCh <- adminSrv.ListenAndServe() }()
    readiness.Store(true)
    slog.Info("listening",
        "addr", cfg.Addr, "admin_addr", cfg.AdminAddr)

    return waitForShutdown(ctx, cfg, publicSrv, adminSrv, readiness, errCh)
}
```

What each phase gives you:

- **A. Config.** No I/O, no network, no logging yet. If the env is broken, we
  fail with a clear message on stderr and a non-zero exit.
- **B. Observability.** Logger before tracer so we can log tracer init failures.
  Tracer before servers so spans can start at the edge. Metrics last because
  they have no init dependencies.
- **C. Handlers.** `ServeMux` is stdlib. The 1.22 pattern syntax
  (`"POST /webhook/{provider}"`) is what lets us skip chi.
- **D. Servers.** Two of them, identical construction, different handlers and
  addresses.
- **E. Run loop.** Each server runs in a goroutine; `errCh` collects their
  results. `readiness.Store(true)` flips `/readyz` green only after both servers
  are listening.

The ordering of the `Chain(...)` calls matters; see §5.

## 3. Configuration: `internal/config`

One struct, one function. No framework.

```go
type Config struct {
    Addr               string
    AdminAddr          string
    ReadTimeout        time.Duration
    ReadHeaderTimeout  time.Duration
    WriteTimeout       time.Duration
    IdleTimeout        time.Duration
    MaxBodyBytes       int64
    ShutdownTimeout    time.Duration
    LogLevel           slog.Level
    LogFormat          string
    SigningSecret      []byte

    TracingEnabled     bool
    TracingSampleRatio float64

    // Standard OTEL_* vars are read by the SDK, but we capture a
    // couple for our own resource assembly.
    ServiceName    string
    ServiceVersion string

    BuildInfo BuildInfo // injected via ldflags in main
}

func Load() (*Config, error) {
    cfg := &Config{
        Addr:              envString("WEBHOOK_ADDR", ":8080"),
        AdminAddr:         envString("WEBHOOK_ADMIN_ADDR", ":9090"),
        ReadTimeout:       envDuration("WEBHOOK_READ_TIMEOUT", 5*time.Second),
        // ... and so on
    }

    secret := os.Getenv("WEBHOOK_SIGNING_SECRET")
    if secret == "" {
        return nil, errors.New("WEBHOOK_SIGNING_SECRET is required")
    }
    cfg.SigningSecret = []byte(secret)

    if cfg.TracingSampleRatio < 0 || cfg.TracingSampleRatio > 1 {
        return nil, fmt.Errorf(
            "WEBHOOK_TRACING_SAMPLE_RATIO out of range: %v",
            cfg.TracingSampleRatio)
    }
    return cfg, nil
}
```

Helpers (`envString`, `envDuration`, `envInt64`, `envBool`) are ten lines each.
No dependency on `envconfig` or `viper`. If you add a new option, add it to the
struct, add a line in `Load`, and add a row to the env-var table in the design
doc.

## 4. Observability Initialization

### 4.1 Logging — `internal/observability/logging.go`

```go
func NewLogger(level slog.Level, format string) *slog.Logger {
    opts := &slog.HandlerOptions{Level: level}
    var base slog.Handler
    if format == "text" {
        base = slog.NewTextHandler(os.Stdout, opts)
    } else {
        base = slog.NewJSONHandler(os.Stdout, opts)
    }
    return slog.New(&traceHandler{Handler: base})
}

type traceHandler struct{ slog.Handler }

func (h *traceHandler) Handle(ctx context.Context, r slog.Record) error {
    if span := trace.SpanFromContext(ctx); span.SpanContext().IsValid() {
        sc := span.SpanContext()
        r.AddAttrs(
            slog.String("trace_id", sc.TraceID().String()),
            slog.String("span_id",  sc.SpanID().String()),
        )
    }
    return h.Handler.Handle(ctx, r)
}
```

The whole "trace-correlated logs" feature is that 12-line handler. Any `slog`
call made with a context that carries an active span will get `trace_id` and
`span_id` attached automatically; any call without a span (or with
`context.Background()`) will emit without them and no error.

The important consequence: **always pass the request context** to logger calls
inside a handler.

```go
// GOOD — correlated
slog.InfoContext(r.Context(), "signature verified", "provider", provider)

// BAD — no trace_id / span_id will appear
slog.Info("signature verified", "provider", provider)
```

### 4.2 Tracing — `internal/observability/tracing.go`

```go
func NewTracerProvider(ctx context.Context, cfg *config.Config) (
    *sdktrace.TracerProvider, error,
) {
    if !cfg.TracingEnabled {
        return sdktrace.NewTracerProvider(), nil // no-op-ish
    }

    exporter, err := otlptracehttp.New(ctx) // reads OTEL_EXPORTER_OTLP_*
    if err != nil {
        return nil, fmt.Errorf("otlp exporter: %w", err)
    }

    res, err := resource.New(ctx,
        resource.WithFromEnv(),   // reads OTEL_RESOURCE_ATTRIBUTES
        resource.WithAttributes(
            semconv.ServiceName(cfg.ServiceName),
            semconv.ServiceVersion(cfg.ServiceVersion),
        ),
    )
    if err != nil {
        return nil, fmt.Errorf("resource: %w", err)
    }

    tp := sdktrace.NewTracerProvider(
        sdktrace.WithBatcher(exporter),
        sdktrace.WithResource(res),
        sdktrace.WithSampler(samplerFor(cfg.TracingSampleRatio)),
    )
    return tp, nil
}

// samplerFor picks the right inner sampler for the ratio. At 1.0 we
// use AlwaysSample rather than TraceIDRatioBased(1.0) — equivalent
// behavior, clearer intent when reading code or traces.
func samplerFor(ratio float64) sdktrace.Sampler {
    switch {
    case ratio >= 1.0:
        return sdktrace.ParentBased(sdktrace.AlwaysSample())
    case ratio <= 0.0:
        return sdktrace.ParentBased(sdktrace.NeverSample())
    default:
        return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))
    }
}
```

Key points:

- **`otlptracehttp.New`** reads the standard `OTEL_EXPORTER_OTLP_*` environment
  — we do not re-parse those.
- **`ParentBased(AlwaysSample())` by default.** At our expected traffic (10–20
  rps), keeping every trace costs effectively nothing and means "find me the
  trace for the delivery that just failed" is always answerable. `ParentBased`
  still honors upstream sampling decisions if a provider ever sends us sampled
  traffic — if they explicitly dropped a trace, we respect that; we only decide
  for root traces. See §8.7 for when to turn this down.
- **Batch processor, not simple.** Simple span processor blocks the request on
  the exporter; a batcher buffers and flushes in the background. The tradeoff is
  that spans can be lost on a hard kill; that is why `main.go`'s deferred
  `tp.Shutdown(ctx)` is important — it's the flush point.
- **Two propagators.** `TraceContext` for W3C `traceparent`; `Baggage` so we can
  carry domain context (tenant, customer id) across services in Phase 2+.

### 4.3 Metrics — `internal/observability/metrics.go`

One registry, one `*Metrics` struct holding all the instruments:

```go
type Metrics struct {
    // HTTP layer
    HTTPRequests       *prometheus.CounterVec
    HTTPDuration       *prometheus.HistogramVec
    HTTPRequestSize    *prometheus.HistogramVec
    HTTPResponseSize   *prometheus.HistogramVec
    HTTPInflight       prometheus.Gauge
    HTTPPanics         prometheus.Counter

    // Webhook domain
    WebhookEvents      *prometheus.CounterVec
    WebhookSigResults  *prometheus.CounterVec
    WebhookProcessing  *prometheus.HistogramVec
}

func NewMetrics(build BuildInfo) (*prometheus.Registry, *Metrics) {
    reg := prometheus.NewRegistry()
    m := &Metrics{
        HTTPRequests: prometheus.NewCounterVec(
            prometheus.CounterOpts{
                Name: "webhookd_http_requests_total",
                Help: "HTTP requests served, labeled by method, route, and status.",
            },
            []string{"method", "route", "status"},
        ),
        HTTPDuration: prometheus.NewHistogramVec(
            prometheus.HistogramOpts{
                Name:    "webhookd_http_request_duration_seconds",
                Help:    "HTTP request latency, labeled by method, route, status.",
                Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
            },
            []string{"method", "route", "status"},
        ),
        // ... the rest follow the same pattern
    }

    reg.MustRegister(
        m.HTTPRequests, m.HTTPDuration, m.HTTPRequestSize,
        m.HTTPResponseSize, m.HTTPInflight, m.HTTPPanics,
        m.WebhookEvents, m.WebhookSigResults, m.WebhookProcessing,
        collectors.NewGoCollector(),
        collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
        buildInfoCollector(build),
    )
    return reg, m
}
```

A few conventions to preserve:

- **Own registry, not `prometheus.DefaultRegisterer`.** Using a local registry
  lets tests create a fresh one per case and prevents "already registered"
  panics in repeated runs.
- **Metric names.** Prefix every custom metric with `webhookd_`. Units in the
  suffix (`_seconds`, `_bytes`, `_total` for counters). Follow the Prometheus
  [instrumentation best practices][prom-best].
- **Bucket choice.** Histograms are expensive per-bucket; the ten buckets here
  are tuned for a sub-second webhook receiver. Do not change them without a
  reason — dashboards and SLOs will drift.
- **Label cardinality.** Every label value added to a histogram multiplies ten
  buckets plus sum and count. A 100-value label on the duration histogram is
  1,200 time series from one handler. Keep labels bounded and enumerable; the
  `route` label uses the ServeMux pattern string, not the URL path.

[prom-best]: https://prometheus.io/docs/practices/instrumentation/

## 5. The Middleware Chain

`httpx.Chain` is a tiny helper:

```go
// Chain wraps h in middlewares, outermost-first.
func Chain(h http.Handler, mws ...func(http.Handler) http.Handler) http.Handler {
    for i := len(mws) - 1; i >= 0; i-- {
        h = mws[i](h)
    }
    return h
}
```

In `main.go` we wrote:

```go
httpx.Chain(publicMux,
    httpx.Recover(metrics),
    httpx.OTel("webhookd"),
    httpx.RequestID(),
    httpx.SLog(),
    httpx.Metrics(metrics),
)
```

Which produces this stack (first in the list is outermost, so runs first on the
way in and last on the way out):

```
  ┌─────────────────────────────────┐
  │ Recover                         │ ← catches panics anywhere below
  │  ┌───────────────────────────┐  │
  │  │ otelhttp (OTel)           │  │ ← server span starts here
  │  │  ┌─────────────────────┐  │  │
  │  │  │ RequestID           │  │  │ ← x-request-id populated
  │  │  │  ┌───────────────┐  │  │  │
  │  │  │  │ SLog          │  │  │  │ ← logs request/response
  │  │  │  │  ┌─────────┐  │  │  │  │
  │  │  │  │  │ Metrics │  │  │  │  │ ← records HTTP counters
  │  │  │  │  │  ┌────┐ │  │  │  │  │
  │  │  │  │  │  │Mux │ │  │  │  │  │
  │  │  │  │  │  └────┘ │  │  │  │  │
  │  │  │  │  └─────────┘  │  │  │  │
  │  │  │  └───────────────┘  │  │  │
  │  │  └─────────────────────┘  │  │
  │  └───────────────────────────┘  │
  └─────────────────────────────────┘
```

### 5.1 Why This Specific Order?

- **Recover outermost.** If any middleware itself panics (a buggy `otelhttp`
  upgrade, say), we still want a clean 500 and a counter increment rather than a
  crashed goroutine.
- **OTel before logging.** `slog` reads the span from context to populate
  `trace_id` / `span_id`. If `otelhttp` is inside `slog`, the span does not
  exist yet when the first log line is emitted.
- **RequestID between OTel and slog.** The log line should carry `request_id`,
  and `otelhttp` should not care about it.
- **Metrics innermost.** We want `status` in our HTTP metrics to reflect what
  the handler actually returned, not what a middleware layered on top later
  (e.g. compression middleware flipping 200 to 204). Innermost sees the truth.

### 5.2 The Metrics Middleware in Detail

```go
func Metrics(m *observability.Metrics) func(http.Handler) http.Handler {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            m.HTTPInflight.Inc()
            defer m.HTTPInflight.Dec()

            start := time.Now()
            rw := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
            route := routeFor(r) // ServeMux pattern, e.g. /webhook/{provider}

            next.ServeHTTP(rw, r)

            labels := prometheus.Labels{
                "method": r.Method,
                "route":  route,
                "status": strconv.Itoa(rw.status),
            }
            m.HTTPRequests.With(labels).Inc()
            m.HTTPDuration.With(labels).Observe(time.Since(start).Seconds())
            m.HTTPRequestSize.With(prometheus.Labels{
                "method": r.Method, "route": route,
            }).Observe(float64(r.ContentLength))
            m.HTTPResponseSize.With(prometheus.Labels{
                "method": r.Method, "route": route,
            }).Observe(float64(rw.bytes))
        })
    }
}
```

`routeFor(r)` is the critical bit that keeps cardinality bounded. On Go 1.22+,
`http.ServeMux` exposes the matched pattern via `r.Pattern`. For unmatched
routes (404s) we substitute `"__unmatched__"` so bots and probes cannot explode
the label space.

## 6. The Request Lifecycle

A worked example: `POST /webhook/stripe` with a valid signature.

```
  t=0ms  TCP accept, listener hands the conn to net/http
         │
         ▼
  t=1ms  Recover middleware: defer recover() registered
         │
         ▼
  t=1ms  otelhttp: tracer.Start(ctx, "POST /webhook/{provider}")
         │     sets HTTP semantic attrs (method, url, user_agent, …)
         │     stores span in r.Context()
         │
         ▼
  t=1ms  RequestID: reads/generates X-Request-ID
         │     puts "req_id" in context via ctxkey
         │
         ▼
  t=1ms  SLog: records start; does not emit yet (we log on return)
         │
         ▼
  t=1ms  Metrics: HTTPInflight.Inc(); captures t0
         │
         ▼
  t=2ms  Handler runs:
         │   ctx := r.Context()
         │   ctx, span := tracer.Start(ctx, "webhook.verify_signature")
         │     body, _ := io.ReadAll(http.MaxBytesReader(w, r.Body, cfg.MaxBodyBytes))
         │     valid := signature.Verify(cfg.SigningSecret, r.Header.Get(...), body)
         │     metrics.WebhookSigResults.With(...).Inc()
         │   span.End()
         │
         │   ctx, span = tracer.Start(ctx, "webhook.parse")
         │     var envelope Envelope; json.Unmarshal(body, &envelope)
         │   span.End()
         │
         │   slog.InfoContext(ctx, "webhook accepted",
         │       "provider", provider, "event_type", envelope.EventType)
         │   metrics.WebhookEvents.With(...).Inc()
         │   metrics.WebhookProcessing.With(...).Observe(...)
         │   w.WriteHeader(http.StatusAccepted)
         │
         ▼
  t=8ms  Metrics middleware on return:
         │     HTTPRequests.With(method,route,status).Inc()
         │     HTTPDuration.With(...).Observe(8ms)
         │     HTTPRequestSize, HTTPResponseSize observed
         │     HTTPInflight.Dec()
         │
         ▼
  t=8ms  SLog on return: emits one INFO line with duration_ms=8,
         │   method, route, status, bytes_in, bytes_out, trace_id, span_id
         │
         ▼
  t=8ms  RequestID on return: no-op (context will GC)
         │
         ▼
  t=8ms  otelhttp: sets span status from rw.status, span.End()
         │   the span enters the batch processor's queue
         │
         ▼
  t=8ms  Recover on return: no-op
         │
         ▼
  t=8ms  Response flushed to provider
         │
         ... later ...
  t=~5s  BatchSpanProcessor flushes a batch to the OTLP collector
```

What emerged from this single request:

- **1 parent span** (`POST /webhook/{provider}`) with HTTP attrs.
- **2 child spans** (`webhook.verify_signature`, `webhook.parse`), each with
  their own timing.
- **1 access log line**, JSON, with `trace_id` and `span_id` matching the parent
  span.
- **1 handler log line** (`"webhook accepted"`) with the same trace context.
- **6 metric observations**: one increment on `HTTPRequests`, one observation
  each on `HTTPDuration`, `HTTPRequestSize`, `HTTPResponseSize`; increment +
  decrement on `HTTPInflight`; increments on `WebhookEvents` and
  `WebhookSigResults`; one observation on `WebhookProcessing`.

## 7. Metrics Deep Dive

### 7.1 What Gets Recorded Where

| Metric                                         | Recorded in                      | When                              |
| ---------------------------------------------- | -------------------------------- | --------------------------------- |
| `webhookd_http_requests_total`                 | `httpx.Metrics` middleware       | End of every request              |
| `webhookd_http_request_duration_seconds`       | `httpx.Metrics`                  | End of every request              |
| `webhookd_http_request_size_bytes`             | `httpx.Metrics`                  | End of every request              |
| `webhookd_http_response_size_bytes`            | `httpx.Metrics`                  | End of every request              |
| `webhookd_http_inflight_requests`              | `httpx.Metrics`                  | `Inc()` on entry, `Dec()` on exit |
| `webhookd_http_panics_total`                   | `httpx.Recover`                  | On `recover()`                    |
| `webhookd_webhook_events_total`                | `webhook.Handler`                | Every terminal outcome            |
| `webhookd_webhook_signature_validation_total`  | `webhook.Handler`                | After HMAC check                  |
| `webhookd_webhook_processing_duration_seconds` | `webhook.Handler`                | End of every processed event      |
| `go_*`                                         | `collectors.NewGoCollector`      | On scrape                         |
| `process_*`                                    | `collectors.NewProcessCollector` | On scrape                         |
| `webhookd_build_info`                          | Constant collector               | On scrape                         |

### 7.2 Reading the Metrics

`curl -s localhost:9090/metrics | head`:

```
# HELP webhookd_http_requests_total HTTP requests served, labeled by method, route, and status.
# TYPE webhookd_http_requests_total counter
webhookd_http_requests_total{method="POST",route="/webhook/{provider}",status="202"} 1

# HELP webhookd_http_request_duration_seconds HTTP request latency, labeled by method, route, status.
# TYPE webhookd_http_request_duration_seconds histogram
webhookd_http_request_duration_seconds_bucket{method="POST",route="/webhook/{provider}",status="202",le="0.005"} 0
webhookd_http_request_duration_seconds_bucket{method="POST",route="/webhook/{provider}",status="202",le="0.01"} 1
...
```

Note how the `route` label is the ServeMux pattern, not the literal URL. This is
deliberate and is the main defense against cardinality blowup.

### 7.3 How to Add a New Metric (Worked Example)

Say we want to track the number of webhook events we've rejected because they
arrived outside a timestamp skew window (a feature we'd add in Phase 2). Four
steps:

**1. Add the instrument to the `Metrics` struct and registry** in
`internal/observability/metrics.go`:

```go
type Metrics struct {
    // ...existing fields...
    WebhookClockSkew *prometheus.CounterVec
}

// inside NewMetrics:
m.WebhookClockSkew = prometheus.NewCounterVec(
    prometheus.CounterOpts{
        Name: "webhookd_webhook_clock_skew_rejected_total",
        Help: "Webhooks rejected because the timestamp was outside the allowed skew window.",
    },
    []string{"provider"},
)

reg.MustRegister(
    // ...existing...
    m.WebhookClockSkew,
)
```

**2. Record it** in the handler:

```go
if skew > cfg.MaxSkew {
    m.WebhookClockSkew.With(prometheus.Labels{"provider": provider}).Inc()
    m.WebhookEvents.With(prometheus.Labels{
        "provider": provider, "event_type": "", "outcome": "clock_skew",
    }).Inc()
    http.Error(w, "clock skew too large", http.StatusBadRequest)
    return
}
```

**3. Add a test** in `internal/webhook/handler_test.go`:

```go
func TestHandler_RejectsClockSkew(t *testing.T) {
    reg, m := observability.NewMetrics(observability.BuildInfo{})
    h := webhook.NewHandler(secret, m)

    req := newRequestWithSkew(2 * time.Hour)
    rr := httptest.NewRecorder()
    h.ServeHTTP(rr, req)

    if got, want := rr.Code, http.StatusBadRequest; got != want {
        t.Fatalf("status = %d, want %d", got, want)
    }
    if got := testutil.ToFloat64(
        m.WebhookClockSkew.WithLabelValues("stripe"),
    ); got != 1 {
        t.Fatalf("WebhookClockSkew = %v, want 1", got)
    }
    _ = reg // keep registry alive
}
```

**4. Update the design doc's metric table** so future readers do not have to
`grep` for what exists.

### 7.4 Cardinality Checklist

Before merging a PR that adds a metric, confirm:

- [ ] Every label value is drawn from a bounded, enumerable set (method names,
      registered routes, fixed outcome strings).
- [ ] No user-supplied input (headers, path params, query strings) becomes a
      label value verbatim.
- [ ] If the metric is a histogram, the buckets are chosen for the actual
      expected range — not the default.
- [ ] The help text names the unit: "seconds", "bytes", "events".

## 8. Tracing Deep Dive

### 8.1 Where Spans Come From

Three sources in Phase 1:

1. **`otelhttp.NewHandler`** creates one root server span per request. You get
   this for free.
2. **`tracer.Start` calls** inside the handler create child spans for business
   operations.
3. **Library instrumentation**, later: when we add an HTTP client to call a
   downstream service in Phase 2, we wrap its `http.Transport` in
   `otelhttp.NewTransport` and get client spans automatically.

### 8.2 How Propagation Works on the Way In

A provider that supports W3C Trace Context includes a header like
`traceparent: 00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01` in its
webhook delivery. The flow on ingress:

1. `otelhttp` reads the configured `TextMapPropagator` (we set it to
   `TraceContext+Baggage`) and extracts the parent span context from
   `traceparent`.
2. It calls `tracer.Start(ctx, ...)`, which, thanks to the `ParentBased`
   sampler, preserves the upstream sampling decision — so if the provider
   sampled the trace, we continue recording; if not, we do not.
3. The returned context carries the active span. Anything downstream (handler,
   child spans, logs) picks it up from the context.

### 8.3 How to Add a Custom Span (Worked Example)

Say the Phase 2 handler will call a downstream service to publish each event. We
want that call traced as a child span.

```go
func (h *Handler) publish(ctx context.Context, evt Event) error {
    tracer := otel.Tracer("webhookd/webhook")
    ctx, span := tracer.Start(ctx, "webhook.publish",
        trace.WithAttributes(
            attribute.String("provider", evt.Provider),
            attribute.String("event_type", evt.EventType),
            attribute.Int("payload_bytes", len(evt.Payload)),
        ),
    )
    defer span.End()

    if err := h.publisher.Publish(ctx, evt); err != nil {
        span.RecordError(err)
        span.SetStatus(codes.Error, "publish failed")
        return fmt.Errorf("publish: %w", err)
    }
    return nil
}
```

Three conventions:

- **Name spans `subsystem.operation`.** Dotted, lowercase, action-oriented.
  `webhook.publish`, not `Publish` or `publishing_the_webhook`.
- **`defer span.End()` immediately.** If the function has multiple return paths
  (it probably does), `defer` is the only way not to leak.
- **Record errors and status explicitly.** Returning an error is not enough;
  OTel has no way to know. `span.RecordError(err)` + a `codes.Error` status is
  the pattern, and it is what makes "show me traces for failed publishes"
  queries work in Tempo or Jaeger.

### 8.4 Span Attributes: Useful vs Bloat

Attributes you almost always want:

- Identifiers of the business object (`provider`, `event_type`, `event_id`,
  `tenant_id`).
- Sizes where they might explain latency (`payload_bytes`, `batch_size`).
- Enum-typed outcomes where they might explain errors (`retry_count`,
  `cache_result`).

Attributes you almost never want:

- Full request or response bodies. Spans are not log storage.
- Secrets, headers, or anything PII-adjacent.
- Unbounded free-form strings (user-agents as-is, URLs including query strings).

### 8.5 Baggage

Baggage is a second propagator for domain context that you want to ride along
with the trace. Example: a `tenant_id` set at the edge that every downstream
service should see as a span attribute.

```go
bag, _ := baggage.New(baggage.NewMemberRaw("tenant_id", tenantID))
ctx := baggage.ContextWithBaggage(r.Context(), bag)

// Downstream, any service can read it:
mem := baggage.FromContext(ctx).Member("tenant_id")
span.SetAttributes(attribute.String("tenant_id", mem.Value()))
```

We do not use baggage in Phase 1 — there is only one service. It is wired up so
Phase 2 can use it immediately.

### 8.6 How Logs Correlate With Traces

The correlation is entirely the `traceHandler` from §4.1: a span lookup on the
record's context adds `trace_id` and `span_id`.

```
                       ┌──────────────────────────────────┐
 incoming request ───► │ otelhttp span starts              │
                       │ ctx now carries span              │
                       └─────┬────────────────────────────┘
                             │ r = r.WithContext(ctx)
                             ▼
                       ┌──────────────────────────────────┐
                       │ slog.InfoContext(r.Context(), …)  │
                       │ traceHandler.Handle() pulls span  │
                       │ from ctx, adds trace_id/span_id   │
                       └─────┬────────────────────────────┘
                             │
                             ▼
                       JSON line on stdout:
                       {
                         "time":"…","level":"INFO","msg":"webhook accepted",
                         "provider":"stripe","event_type":"charge.succeeded",
                         "trace_id":"0af7651916cd43dd8448eb211c80319c",
                         "span_id":"b7ad6b7169203331"
                       }
```

Grafana's Loki data source can derive a trace link from the `trace_id` field
directly to a Tempo query. No extra infra needed.

### 8.7 Sampling in Practice

**Our Phase 1 posture: keep every trace.** We run with
`WEBHOOK_TRACING_SAMPLE_RATIO=1.0`, which resolves to
`ParentBased(AlwaysSample())`. At 10–20 rps steady state the volume is trivial —
the BatchSpanProcessor queue barely moves, the OTLP exporter has plenty of
headroom, and storage cost at the collector is a rounding error. More
importantly, the way we actually _use_ traces for this service is reactive: a
specific webhook delivery failed or behaved oddly, we have its `trace_id` in a
log line, we want to see that trace. That workflow only works if the trace
exists. Dropping 90% of traces to save resources we aren't spending would be
pure downside.

So: default is keep-everything, and the ratio knob is a safety valve, not an
operating mode.

**When sampling becomes worth considering.** The decision is usually driven by
one of three pressures:

- **Volume.** Somewhere in the low-hundreds-rps range, per-trace cost starts to
  matter: more collector CPU, more OTLP egress, more ingested spans at the
  backend. The exact inflection depends on your collector topology and backend
  pricing; there's no magic number. When traces-per-day starts showing up on a
  billing dashboard, it's time to talk about sampling.
- **Cardinality of interesting traces.** If 99% of traffic is uniform happy-path
  requests and you can see everything you need from metrics, a ratio sampler
  wastes storage on redundant traces. This is the classic ratio-sampling
  argument.
- **Tail latency visibility.** Head-based ratio sampling is blind to outliers —
  if you sample at 10% and a rare 5-second request happens, there's a 90% chance
  you didn't keep it. Tail-based sampling (done in the collector, not the SDK)
  solves this by deciding _after_ the trace completes whether to keep it. Tail
  sampling is a Phase 3+ discussion for this service.

**What to do if volume changes.** The escalation path:

1. **Turn the knob down gradually.** Drop to `0.5`, then `0.1`.
   `ParentBased(TraceIDRatioBased(ratio))` takes over transparently; no code
   changes. Monitor collector and backend cost.
2. **Bias toward errors.** When ratio sampling is active, you still want to keep
   every failed request. In Phase 2 we'd add a custom sampler that force-samples
   spans flagged with `span.SetStatus(codes.Error, ...)` regardless of ratio.
   The pattern is a composed sampler in `samplerFor`.
3. **Move sampling to the collector.** If head-based sampling is leaving blind
   spots (rare but high-value outliers), shift to tail-based in the OTel
   collector. The SDK then samples at 1.0 and the collector makes the drop/keep
   decision with full trace context. This costs collector resources but wins on
   signal quality.

**Useful sanity checks at any ratio:**

```promql
# Traces accepted by the collector vs dropped — watch for queue saturation.
rate(otelcol_processor_batch_batch_send_size_sum[5m])
rate(otelcol_exporter_send_failed_spans[5m])
```

If you ever do turn the ratio below 1.0, remember that `trace_id` is still
emitted into logs for every request, whether the trace was sampled or not.
Sampled vs not sampled only determines whether spans are _exported_; the trace
ID is generated unconditionally. This means a log line can carry a `trace_id`
that has no corresponding trace in Tempo — confusing the first time you see it.
If that becomes a routine source of dead-end debugging, that's a signal the
sampling ratio is too aggressive for the service's workflow, and the first-line
fix is raising the ratio rather than adding more logging.

**Reference table for the ratio config:**

| `WEBHOOK_TRACING_SAMPLE_RATIO` | Resolved sampler                    | Use case                                                  |
| ------------------------------ | ----------------------------------- | --------------------------------------------------------- |
| `1.0` _(default)_              | `ParentBased(AlwaysSample())`       | Low-volume services; debugging by trace ID                |
| `0.1` – `0.9`                  | `ParentBased(TraceIDRatioBased(r))` | Mid-volume services managing cost                         |
| `0.0`                          | `ParentBased(NeverSample())`        | Emergency kill-switch; trace IDs still generated for logs |

## 9. Graceful Shutdown

On `SIGTERM` the shutdown sequence is:

1. **Flip `/readyz` to fail.** The LB's next readiness check (worst case:
   `readinessProbe.periodSeconds`) stops sending new traffic.
2. **`srv.Shutdown(ctx)` on both servers.** New connections rejected; in-flight
   requests given `WEBHOOK_SHUTDOWN_TIMEOUT` to finish.
3. **`tp.Shutdown(ctx)` on the tracer provider.** Batch processor flushes any
   queued spans to the OTLP endpoint.
4. **Exit 0.** If step 2 or 3 exceeded the budget, exit 1 so the orchestrator
   knows the drain was dirty.

The PreStop hook in Kubernetes should sleep for a readiness interval before
sending SIGTERM, which gives step 1 time to propagate. This is a deployment
concern, not a service concern, but flag it on the Kubernetes manifest when
rolling out.

## 10. Operational Quick Reference

**Prometheus ServiceMonitor selector:**

```yaml
matchLabels:
  app.kubernetes.io/name: webhookd
podMetricsEndpoints:
  - port: admin # :9090
    path: /metrics
    interval: 30s
```

**Key PromQL queries:**

- Request rate by status:
  `sum by (status) (rate(webhookd_http_requests_total[5m]))`
- p99 latency:
  `histogram_quantile(0.99, sum by (le) (rate(webhookd_http_request_duration_seconds_bucket[5m])))`
- Signature failures:
  `sum(rate(webhookd_webhook_signature_validation_total{result="invalid"}[5m]))`
- Inflight requests gauge trend: `webhookd_http_inflight_requests`
- Panic rate: `sum(rate(webhookd_http_panics_total[5m]))`

**Recommended alerts (starter set):**

- `webhookd_http_panics_total` increases at all → page.
- `histogram_quantile(0.99, ...)` p99 > 1s for 5m → page.
- `webhookd_http_requests_total{status=~"5.."}` rate > 1% of total for 5m →
  page.
- `webhookd_webhook_signature_validation_total{result="invalid"}` rate spike
  (e.g. >10/s) → ticket (likely misconfigured sender or attack).
- `up{job="webhookd"} == 0` → page.
- `absent(webhookd_build_info)` → page (service never scraped).

**Troubleshooting:**

- _No traces showing up._ Check `WEBHOOK_TRACING_ENABLED`, the OTLP endpoint
  reachability from the pod, and the collector logs. A collector dropping
  invalid traces is silent by default — enable debug logging there first.
- _High cardinality alert firing on Prometheus._ Grep for any
  `With(prometheus.Labels{...})` call that takes a user-controlled value; that
  is the one.
- _`/readyz` never turns green._ The `atomic.Bool` flip is after both
  `ListenAndServe` calls are dispatched. If one fails to bind, check the error
  on `errCh`.

## 11. What This Phase Intentionally Leaves Out

Briefly, because the design doc covers it, but worth re-stating as you read the
code:

- **No retry, no queue, no persistence.** If we return 5xx, the provider
  retries. If the provider does not retry, the event is gone. Phase 2.
- **No OTel metrics.** Prometheus owns metrics in Phase 1. We will revisit
  unifying onto the OTel SDK once it is clear whether our metric backends
  benefit from it.
- **One provider, one signing secret.** The handler is generic over `{provider}`
  but the verification key is singular. Multi-provider secret management is
  Phase 2.
- **No admin-listener auth.** In-cluster only. Revisit if it moves.
- **No rate limiting.** Assume upstream provider volume is bounded and
  Kubernetes HPA absorbs bursts. Revisit if a provider ever floods us.

The shape of the codebase is specifically chosen so each of those can be added
without re-plumbing the observability layer. That is the whole point of putting
the observability substrate first.
