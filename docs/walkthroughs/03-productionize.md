# Walkthrough: Productionize the typed `note-api` (Part 3)

[Part 2](02-typed-clients-with-kubebuilder.md) left us with a typed webhook
receiver: ~150 lines of Go, a sibling kubebuilder operator project that owns the
CRD types, and a `docker buildx bake` pipeline. It works, but it would not
survive contact with production.

This walkthrough adds the operational layers a real service needs, one at a
time:

1. **Structured logging** with `log/slog`, JSON output, and a request-id
   correlated across log lines.
2. **Graceful shutdown** — drain in-flight requests on SIGTERM instead of
   dropping them.
3. **Proper error classification** — map `IsForbidden`, `IsInvalid`,
   `IsAlreadyExists`, etc. to the right HTTP status codes.
4. **Prometheus metrics** on a separate admin port.
5. **OpenTelemetry traces** exported via OTLP to a collector.
6. **A `docker compose` stack** wiring `note-api` + OTel collector +
   Prometheus + Tempo + Grafana so you can watch a request fan out across logs,
   metrics, and traces in real time.

Each layer is small in isolation; the value is in composition. By the end you
will have something that looks operationally close to this repo's actual
`webhookd` — minus signing, rate limiting, and the sync watch loop, all of which
warrant their own designs (see `docs/design/`).

The full final files are in the [Appendix](#appendix-full-files) at the bottom —
copy-paste anchors so you can always jump straight to the working state.

---

## What we are NOT changing

- **The kubebuilder operator project** from Part 2. `note-operator` is
  unchanged. Productionization is an api concern.
- **The `Note` CRD shape.** Same fields, same group/version.
- **The Dockerfile and `docker-bake.hcl`.** The image stays distroless; the
  build pipeline still runs `docker buildx bake`. We add a few ports and config
  volumes at runtime, not at build time.

So everything in this walkthrough lives in `note-api/main.go`, plus new sibling
files for the local stack.

---

## Step 1 — Structured logging with `log/slog`

`log.Printf` from Parts 1-2 dumps unstructured strings to stderr — fine for a
tutorial, useless when you're correlating logs across services. Stdlib
`log/slog` (Go 1.21+) gives us JSON output with typed fields and zero new
dependencies.

We want every log line for a request to carry the same `request_id`, generated
at the front door and propagated via `context.Context`.

### 1a — A request-id middleware

Add this to `main.go`:

```go
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

type ctxKey int

const requestIDKey ctxKey = 0

// newRequestID returns a 16-hex-char random identifier. crypto/rand keeps
// this collision-resistant without dragging in github.com/google/uuid.
func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// requestID middleware reads X-Request-ID if the caller supplied one,
// or generates a fresh ID. Either way it stashes it in ctx and echoes
// it on the response. Downstream handlers and loggers pull it via
// requestIDFromContext.
func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set("X-Request-ID", id)
		ctx := context.WithValue(r.Context(), requestIDKey, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func requestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey).(string); ok {
		return v
	}
	return ""
}
```

### 1b — A `slog` logger that attaches request-id automatically

```go
import "log/slog"

// loggerFromContext returns a *slog.Logger pre-bound with the request's
// ID and any other contextual attributes. Use this inside handlers
// rather than the package-level slog.Default() — it keeps every log
// line for a request grep-able by ID.
func loggerFromContext(ctx context.Context) *slog.Logger {
	return slog.Default().With(
		slog.String("request_id", requestIDFromContext(ctx)),
	)
}
```

### 1c — Wire it into `main`

```go
import "os"

func main() {
	// JSON output, includes timestamps and source: file:line for ERROR.
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		AddSource: true,
		Level:     slog.LevelInfo,
	})))

	s, err := NewServer()
	if err != nil {
		slog.Error("server init", slog.Any("error", err))
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.Handle("/webhook", requestIDMiddleware(http.HandlerFunc(s.handleWebhook)))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	addr := ":" + envOr("PORT", "8080")
	slog.Info("starting server", slog.String("addr", addr))
	if err := http.ListenAndServe(addr, mux); err != nil {
		slog.Error("listen", slog.Any("error", err))
		os.Exit(1)
	}
}
```

### 1d — Use it in the handler

Inside `handleWebhook`, replace `log.Printf` with the contextual logger:

```go
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	logger := loggerFromContext(r.Context())
	logger.Info("webhook received",
		slog.String("method", r.Method),
		slog.String("path", r.URL.Path),
	)

	// ... decode + validate as before ...

	if err := s.k8s.Create(ctx, note); err != nil {
		logger.Error("create note",
			slog.String("name", note.Name),
			slog.Any("error", err),
		)
		// status code mapping comes in Step 3
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
		return
	}

	logger.Info("created note",
		slog.String("name", note.Name),
		slog.String("namespace", note.Namespace),
		slog.String("uid", string(note.UID)),
	)
	// ... write response as before ...
}
```

Smoke test:

```bash
go run .

curl -X POST -H 'Content-Type: application/json' \
  -d '{"title":"slog","body":"hi"}' localhost:8080/webhook

# Server output (one JSON object per line):
# {"time":"2026-...","level":"INFO","source":{...},"msg":"starting server","addr":":8080"}
# {"time":"2026-...","level":"INFO","source":{...},"msg":"webhook received","request_id":"a3f9...","method":"POST","path":"/webhook"}
# {"time":"2026-...","level":"INFO","source":{...},"msg":"created note","request_id":"a3f9...","name":"slog","namespace":"default","uid":"..."}
```

Same `request_id` on both lines — that's the win. In Grafana / Loki / your log
aggregator of choice, you can filter by `request_id` and see exactly the lines
for one webhook.

> **Why crypto/rand for the ID?** `math/rand` is seeded from the wall clock by
> default — two replicas booting at the same time would generate colliding
> sequences. `crypto/rand` reads from the OS entropy pool (`/dev/urandom` on
> Linux) and never collides at this scale.

---

## Step 2 — Graceful shutdown

Bare `http.ListenAndServe` does not return until the process is SIGKILLed.
SIGTERM, which is what Kubernetes sends on pod termination, just kills
mid-flight requests. We want:

1. SIGTERM arrives → stop accepting new connections.
2. In-flight handlers run to completion within a budget (say 25s).
3. After the budget, fall back to a hard close.

Stdlib `http.Server.Shutdown(ctx)` does exactly this. The pattern:

```go
import (
	"context"
	"errors"
	"os/signal"
	"syscall"
	"time"
)

// realMain returns the exit code so deferred cleanups (logger flush,
// trace provider shutdown in Step 5) can actually run before
// os.Exit. Wrapping main like this is a small habit that pays off
// every time you add a new defer.
func realMain() int {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		AddSource: true,
		Level:     slog.LevelInfo,
	})))

	// signal.NotifyContext returns a context that is cancelled when
	// any of the listed signals fires. SIGINT (Ctrl-C) for dev,
	// SIGTERM for k8s pod termination.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	s, err := NewServer()
	if err != nil {
		slog.Error("server init", slog.Any("error", err))
		return 1
	}

	mux := http.NewServeMux()
	mux.Handle("/webhook", requestIDMiddleware(http.HandlerFunc(s.handleWebhook)))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:              ":" + envOr("PORT", "8080"),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second, // mitigates Slowloris
	}

	// Run the server in a goroutine so we can listen for ctx
	// cancellation in the foreground.
	errCh := make(chan error, 1)
	go func() {
		slog.Info("starting server", slog.String("addr", srv.Addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		slog.Error("listen", slog.Any("error", err))
		return 1
	case <-ctx.Done():
		slog.Info("shutdown signal received, draining")
	}

	// Detach the shutdown context from ctx — we WANT the drain budget
	// to outlast the signal that triggered shutdown.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("graceful shutdown failed, forcing close", slog.Any("error", err))
		_ = srv.Close()
		return 1
	}

	slog.Info("shutdown complete")
	return 0
}

func main() {
	os.Exit(realMain())
}
```

Test the drain behavior locally:

```bash
go run . &
PID=$!

# Send a slow-ish request (the create itself is fast; in real life this
# proves the point on heavy work).
curl -X POST -H 'Content-Type: application/json' \
  -d '{"title":"drain","body":"test"}' localhost:8080/webhook &

# Then immediately:
kill -TERM $PID

# Expected log sequence:
# "shutdown signal received, draining"
# (in-flight curl finishes with 201)
# "shutdown complete"
```

> **Why detach the shutdown context?** The signal-cancelled `ctx` is already
> done by the time we reach the shutdown branch. Deriving the drain ctx from a
> fresh `context.Background()` gives the drain its own budget. This is the same
> pattern this repo's `cmd/webhookd/main.go` uses — an early refactor mistake we
> caught in IMPL-0001.

---

## Step 3 — Proper error classification

`http.StatusInternalServerError` for every K8s failure is a production smell.
Callers cannot tell "your input is invalid" (don't retry) from "we ran out of
capacity" (retry with backoff). Worse, ops dashboards see 500 spikes that are
actually 422s.

`k8s.io/apimachinery/pkg/api/errors` exports predicates for every class of
apiserver error. Map them once, use the result everywhere:

```go
import apierrors "k8s.io/apimachinery/pkg/api/errors"

// resultKind classifies the outcome of a K8s call. We tag
// metrics + traces with this and translate it to an HTTP status
// at the response edge. The strings are stable label values for
// Prometheus.
type resultKind string

const (
	resultOK            resultKind = "ok"
	resultBadRequest    resultKind = "bad_request"
	resultForbidden     resultKind = "forbidden"
	resultConflict      resultKind = "conflict"
	resultInvalid       resultKind = "invalid"
	resultTimeout       resultKind = "timeout"
	resultUnavailable   resultKind = "unavailable"
	resultRateLimited   resultKind = "rate_limited"
	resultUnknown       resultKind = "unknown"
)

// classifyK8sErr maps an apierrors error to a resultKind. The order
// matters: more-specific predicates first.
func classifyK8sErr(err error) resultKind {
	switch {
	case err == nil:
		return resultOK
	case apierrors.IsAlreadyExists(err):
		return resultConflict
	case apierrors.IsInvalid(err):
		return resultInvalid
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		return resultForbidden
	case apierrors.IsTooManyRequests(err):
		return resultRateLimited
	case apierrors.IsTimeout(err), apierrors.IsServerTimeout(err):
		return resultTimeout
	case apierrors.IsServiceUnavailable(err):
		return resultUnavailable
	default:
		return resultUnknown
	}
}

// httpStatus maps the classification to an HTTP status code for the
// response edge.
func (r resultKind) httpStatus() int {
	switch r {
	case resultOK:
		return http.StatusCreated
	case resultBadRequest:
		return http.StatusBadRequest
	case resultConflict:
		return http.StatusConflict
	case resultInvalid:
		return http.StatusUnprocessableEntity
	case resultForbidden:
		return http.StatusForbidden
	case resultRateLimited:
		return http.StatusTooManyRequests
	case resultTimeout:
		return http.StatusGatewayTimeout
	case resultUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}
```

Update `handleWebhook` to use it:

```go
if err := s.k8s.Create(ctx, note); err != nil {
	rk := classifyK8sErr(err)
	logger.Error("create note",
		slog.String("name", note.Name),
		slog.String("result", string(rk)),
		slog.Any("error", err),
	)
	http.Error(w,
		fmt.Sprintf(`{"error":%q,"result":%q}`, err.Error(), rk),
		rk.httpStatus(),
	)
	return
}
```

Now if the cluster denies your ServiceAccount the `notes.examples.dev` verb, the
caller gets a clean `403 Forbidden` and a `"result":"forbidden"` payload — not
a 500.

---

## Step 4 — Prometheus metrics on a separate admin port

Two things matter:

1. **The metrics live on a different port** (`:9091`) than the webhook itself
   (`:8080`). A leaked metrics endpoint is far less damaging than a leaked write
   endpoint, and you can firewall them independently.
2. **Use a private registry**, not `prometheus.DefaultRegisterer`. Tests can
   spin up isolated registries; nothing in your binary leaks across modules.

```bash
go get github.com/prometheus/client_golang@v1.20.5
```

```go
import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// metrics groups every instrument the api emits. Constructing the
// registry locally (not via DefaultRegisterer) means the binary can
// boot multiple times in tests without "duplicate metric" panics.
type metrics struct {
	registry *prometheus.Registry

	webhooksReceived  *prometheus.CounterVec
	webhookDuration   *prometheus.HistogramVec
	k8sCreate         *prometheus.CounterVec
}

func newMetrics() *metrics {
	r := prometheus.NewRegistry()
	r.MustRegister(prometheus.NewGoCollector())
	r.MustRegister(prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))

	m := &metrics{
		registry: r,
		webhooksReceived: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "noteapi_webhooks_received_total",
				Help: "Total webhooks received, partitioned by result.",
			},
			[]string{"result"},
		),
		webhookDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "noteapi_webhook_duration_seconds",
				Help:    "Webhook handler latency, partitioned by result.",
				Buckets: prometheus.DefBuckets,
			},
			[]string{"result"},
		),
		k8sCreate: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "noteapi_k8s_create_total",
				Help: "Note CRD creation attempts, partitioned by outcome.",
			},
			[]string{"outcome"},
		),
	}
	r.MustRegister(m.webhooksReceived, m.webhookDuration, m.k8sCreate)
	return m
}

// adminHandler returns the http.Handler the admin server serves —
// just /metrics and /healthz. No webhook surface.
func (m *metrics) adminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}
```

Wire it into `realMain` (next to the webhook server) and observe in the handler:

```go
// in realMain, after creating Server s:
m := newMetrics()
s.metrics = m // add a *metrics field to Server

adminSrv := &http.Server{
	Addr:              ":" + envOr("ADMIN_PORT", "9091"),
	Handler:           m.adminHandler(),
	ReadHeaderTimeout: 5 * time.Second,
}
go func() {
	slog.Info("starting admin server", slog.String("addr", adminSrv.Addr))
	if err := adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("admin listen", slog.Any("error", err))
	}
}()
defer adminSrv.Shutdown(context.Background()) // best-effort
```

In the handler, observe with a deferred timer + `Inc` per outcome:

```go
func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rk := resultUnknown
	defer func() {
		s.metrics.webhooksReceived.WithLabelValues(string(rk)).Inc()
		s.metrics.webhookDuration.WithLabelValues(string(rk)).Observe(time.Since(start).Seconds())
	}()

	// ... decode + validate; on bad input set rk = resultBadRequest and return ...

	err := s.k8s.Create(ctx, note)
	rk = classifyK8sErr(err)
	s.metrics.k8sCreate.WithLabelValues(string(rk)).Inc()

	if err != nil {
		// ... error response ...
		return
	}

	// ... success response ...
}
```

Smoke test:

```bash
curl -s localhost:9091/metrics | grep noteapi_
# noteapi_webhooks_received_total{result="ok"} 3
# noteapi_webhook_duration_seconds_bucket{result="ok",le="0.005"} 0
# noteapi_webhook_duration_seconds_bucket{result="ok",le="0.01"} 1
# ...
# noteapi_k8s_create_total{outcome="ok"} 3
```

---

## Step 5 — OpenTelemetry traces via OTLP

Traces show _where_ time goes inside a request — handler decode → K8s Create →
response writer. Prometheus tells you "things slowed down"; traces tell you
which part.

Stack: api → otel-collector (OTLP/HTTP) → Tempo. We use the
[contrib collector](https://github.com/open-telemetry/opentelemetry-collector-contrib)
because it has the receiver/exporter set most setups need.

```bash
go get go.opentelemetry.io/otel@v1.30.0
go get go.opentelemetry.io/otel/sdk@v1.30.0
go get go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp@v1.30.0
go get go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp@v0.55.0
```

### 5a — Tracer-provider bootstrap

```go
import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// initTracer wires an OTLP/HTTP exporter to the configured collector
// endpoint and registers a tracer provider as the global. Returns a
// shutdown func — caller MUST call it before exit so the batch
// processor flushes pending spans.
func initTracer(ctx context.Context) (func(context.Context) error, error) {
	endpoint := envOr("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4318")
	exporter, err := otlptrace.New(ctx,
		otlptracehttp.NewClient(
			otlptracehttp.WithEndpointURL(endpoint+"/v1/traces"),
			otlptracehttp.WithTimeout(5*time.Second),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("otlp trace exporter: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName("note-api"),
			semconv.ServiceVersion(envOr("VERSION", "dev")),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("trace resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		// AlwaysSample for the tutorial; production sampling
		// usually a parent-based ratio.
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	return tp.Shutdown, nil
}
```

### 5b — Wrap the webhook handler in OTel HTTP middleware

`otelhttp.NewHandler` automatically creates a span per request, extracts
incoming `traceparent` headers, and ties everything to the global tracer
provider:

```go
import "go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

mux := http.NewServeMux()
mux.Handle("/webhook",
	otelhttp.NewHandler(
		requestIDMiddleware(http.HandlerFunc(s.handleWebhook)),
		"POST /webhook",
	),
)
```

### 5c — Inner span around the K8s call

```go
import "go.opentelemetry.io/otel/attribute"

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	// ... existing setup ...

	tracer := otel.Tracer("note-api")
	ctx, span := tracer.Start(ctx, "k8s.create",
		trace.WithAttributes(
			attribute.String("k8s.resource.kind", "Note"),
			attribute.String("k8s.resource.name", note.Name),
			attribute.String("k8s.resource.namespace", note.Namespace),
		),
	)
	err := s.k8s.Create(ctx, note)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()

	// ... rest of handler ...
}
```

### 5d — Wire shutdown

In `realMain`:

```go
shutdownTracer, err := initTracer(ctx)
if err != nil {
	slog.Error("init tracer", slog.Any("error", err))
	return 1
}
defer func() {
	flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := shutdownTracer(flushCtx); err != nil {
		slog.Error("trace shutdown", slog.Any("error", err))
	}
}()
```

> **Why a separate flush context?** The batch processor holds spans in memory
> and emits them on a timer. If the process exits before the timer fires, those
> spans are lost. `tp.Shutdown(ctx)` flushes them synchronously — but only if
> the context isn't already cancelled. Same pattern as the graceful-shutdown
> drain.

---

## Step 6 — The `docker compose` stack

Five services, all in one `docker-compose.yaml` next to the `note-api/`
directory:

```yaml
# docker-compose.yaml
services:
  note-api:
    build:
      context: ./note-api
    ports:
      - "8080:8080" # webhook
      - "9091:9091" # admin (metrics, healthz)
    environment:
      - PORT=8080
      - ADMIN_PORT=9091
      - OTEL_EXPORTER_OTLP_ENDPOINT=http://otel-collector:4318
      - KUBECONFIG=/kube/config
    volumes:
      # Mount your local kubeconfig so the api can hit your kind cluster.
      - ${HOME}/.kube/config:/kube/config:ro
    depends_on:
      - otel-collector

  otel-collector:
    image: otel/opentelemetry-collector-contrib:0.110.0
    command: ["--config=/etc/otel/config.yaml"]
    volumes:
      - ./compose/otel-config.yaml:/etc/otel/config.yaml:ro
    ports:
      - "4318:4318" # OTLP/HTTP receiver
    depends_on:
      - tempo

  tempo:
    image: grafana/tempo:2.6.0
    command: ["-config.file=/etc/tempo/config.yaml"]
    volumes:
      - ./compose/tempo.yaml:/etc/tempo/config.yaml:ro
    ports:
      - "3200:3200" # query API (Grafana data source)

  prometheus:
    image: prom/prometheus:v2.55.0
    command:
      - "--config.file=/etc/prometheus/prometheus.yml"
      - "--storage.tsdb.path=/prometheus"
    volumes:
      - ./compose/prometheus.yml:/etc/prometheus/prometheus.yml:ro
    ports:
      - "9090:9090"

  grafana:
    image: grafana/grafana:11.3.0
    environment:
      - GF_AUTH_ANONYMOUS_ENABLED=true
      - GF_AUTH_ANONYMOUS_ORG_ROLE=Admin
      - GF_FEATURE_TOGGLES_ENABLE=traceqlEditor
    volumes:
      - ./compose/grafana/datasources:/etc/grafana/provisioning/datasources:ro
    ports:
      - "3000:3000"
    depends_on:
      - prometheus
      - tempo
```

### 6a — `compose/otel-config.yaml`

```yaml
receivers:
  otlp:
    protocols:
      http:
        endpoint: 0.0.0.0:4318

processors:
  batch:
    timeout: 5s

exporters:
  otlp/tempo:
    endpoint: tempo:4317
    tls:
      insecure: true

service:
  pipelines:
    traces:
      receivers: [otlp]
      processors: [batch]
      exporters: [otlp/tempo]
```

### 6b — `compose/tempo.yaml`

```yaml
server:
  http_listen_port: 3200

distributor:
  receivers:
    otlp:
      protocols:
        grpc:
          endpoint: 0.0.0.0:4317

storage:
  trace:
    backend: local
    local:
      path: /tmp/tempo/blocks
    wal:
      path: /tmp/tempo/wal
```

### 6c — `compose/prometheus.yml`

```yaml
global:
  scrape_interval: 5s

scrape_configs:
  - job_name: note-api
    static_configs:
      - targets: ["note-api:9091"]
```

### 6d — `compose/grafana/datasources/datasources.yaml`

```yaml
apiVersion: 1
datasources:
  - name: Prometheus
    type: prometheus
    access: proxy
    url: http://prometheus:9090
    isDefault: true
  - name: Tempo
    type: tempo
    access: proxy
    url: http://tempo:3200
    jsonData:
      tracesToLogs:
        datasourceUid: prometheus
```

---

## Step 7 — Run the whole thing

```bash
# From the project root (sibling to note-api/):
docker compose up --build
```

Once `Started note-api` shows up in the logs:

```bash
# 1. Send a few webhooks.
for i in 1 2 3; do
  curl -s -X POST localhost:8080/webhook \
    -H 'Content-Type: application/json' \
    -d "{\"title\":\"compose-$i\",\"body\":\"observability\"}"
  echo
done

# 2. Verify the CRs landed.
kubectl get notes
# NAME         AGE
# compose-1    3s
# compose-2    2s
# compose-3    1s
```

### Where to look

| URL                             | What you'll see                                                |
| ------------------------------- | -------------------------------------------------------------- |
| <http://localhost:8080/healthz> | `ok` (webhook port)                                            |
| <http://localhost:9091/metrics> | Raw Prometheus exposition (`noteapi_*`)                        |
| <http://localhost:9090/graph>   | Prometheus UI; query `noteapi_webhooks_received_total`         |
| <http://localhost:3000>         | Grafana (anonymous Admin); Explore → Tempo → search `note-api` |

In Grafana: **Explore** → pick **Tempo** → **Search** →
`Service Name = note-api`. You should see one trace per webhook with two spans:
`POST /webhook` (parent) and `k8s.create` (child).

---

## Where to go from here

This walkthrough's `note-api` is now operationally complete: structured logs
with request-id correlation, graceful shutdown, classified errors, Prometheus
metrics, OpenTelemetry traces, and a local observability stack. It is still a
tutorial — production reality adds:

- **Authn/authz on the webhook.** See `internal/webhook/signature.go` in this
  repo for HMAC-SHA256 with replay protection.
- **Rate limiting per source.** See `internal/httpx/ratelimit.go` for
  per-provider token-bucket limiters.
- **Synchronous CR-readiness watch.** See `internal/webhook/executor.go` for an
  SSA Patch + namespace-scoped Watch loop that surfaces operator readiness back
  to the caller in one HTTP round-trip.
- **Server-Side Apply for idempotency.** See ADR-0005 for why a plain `Create`
  is the wrong primitive for retried webhooks.
- **Trace propagation across process boundaries.** See ADR-0007 for the
  W3C-trace-id-on-an-annotation pattern that lets webhookd's span link an
  operator's reconcile span as a remote parent.

Each of those is a small follow-up, and now you have the substrate to add them.

---

## Appendix — full files

The complete final state, ready to copy-paste into a clean directory.

### `note-api/main.go`

```go
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	notev1alpha1 "github.com/example/note-operator/api/v1alpha1"
)

// --- types ---

type CreateNoteRequest struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

type Server struct {
	k8s     client.Client
	metrics *metrics
}

type ctxKey int

const requestIDKey ctxKey = 0

type resultKind string

const (
	resultOK          resultKind = "ok"
	resultBadRequest  resultKind = "bad_request"
	resultForbidden   resultKind = "forbidden"
	resultConflict    resultKind = "conflict"
	resultInvalid     resultKind = "invalid"
	resultTimeout     resultKind = "timeout"
	resultUnavailable resultKind = "unavailable"
	resultRateLimited resultKind = "rate_limited"
	resultUnknown     resultKind = "unknown"
)

// --- scheme ---

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(notev1alpha1.AddToScheme(scheme))
}

// --- main ---

func main() { os.Exit(realMain()) }

func realMain() int {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		AddSource: true,
		Level:     slog.LevelInfo,
	})))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	shutdownTracer, err := initTracer(ctx)
	if err != nil {
		slog.Error("init tracer", slog.Any("error", err))
		return 1
	}
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTracer(flushCtx); err != nil {
			slog.Error("trace shutdown", slog.Any("error", err))
		}
	}()

	m := newMetrics()
	s, err := NewServer(m)
	if err != nil {
		slog.Error("server init", slog.Any("error", err))
		return 1
	}

	mux := http.NewServeMux()
	mux.Handle("/webhook",
		otelhttp.NewHandler(
			requestIDMiddleware(http.HandlerFunc(s.handleWebhook)),
			"POST /webhook",
		),
	)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:              ":" + envOr("PORT", "8080"),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	adminSrv := &http.Server{
		Addr:              ":" + envOr("ADMIN_PORT", "9091"),
		Handler:           m.adminHandler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 2)
	go func() {
		slog.Info("starting server", slog.String("addr", srv.Addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	go func() {
		slog.Info("starting admin server", slog.String("addr", adminSrv.Addr))
		if err := adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		slog.Error("listen", slog.Any("error", err))
		return 1
	case <-ctx.Done():
		slog.Info("shutdown signal received, draining")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("graceful shutdown failed", slog.Any("error", err))
		_ = srv.Close()
	}
	_ = adminSrv.Shutdown(shutdownCtx)
	slog.Info("shutdown complete")
	return 0
}

// --- server / k8s ---

func NewServer(m *metrics) (*Server, error) {
	cfg, err := loadKubeConfig()
	if err != nil {
		return nil, err
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("typed client: %w", err)
	}
	return &Server{k8s: c, metrics: m}, nil
}

func loadKubeConfig() (*rest.Config, error) {
	cfg, err := rest.InClusterConfig()
	if err == nil {
		return cfg, nil
	}
	if !errors.Is(err, rest.ErrNotInCluster) {
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{},
	).ClientConfig()
}

// --- webhook handler ---

func (s *Server) handleWebhook(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rk := resultUnknown
	defer func() {
		s.metrics.webhooksReceived.WithLabelValues(string(rk)).Inc()
		s.metrics.webhookDuration.WithLabelValues(string(rk)).Observe(time.Since(start).Seconds())
	}()

	logger := loggerFromContext(r.Context())
	logger.Info("webhook received", slog.String("method", r.Method))

	if r.Method != http.MethodPost {
		rk = resultBadRequest
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	var req CreateNoteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		rk = resultBadRequest
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	if req.Title == "" || req.Body == "" {
		rk = resultBadRequest
		http.Error(w, `{"error":"title and body are required"}`, http.StatusBadRequest)
		return
	}

	note := &notev1alpha1.Note{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sanitize(req.Title),
			Namespace: envOr("NAMESPACE", "default"),
		},
		Spec: notev1alpha1.NoteSpec{
			Title: req.Title,
			Body:  req.Body,
		},
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	tracer := otel.Tracer("note-api")
	ctx, span := tracer.Start(ctx, "k8s.create",
		trace.WithAttributes(
			attribute.String("k8s.resource.kind", "Note"),
			attribute.String("k8s.resource.name", note.Name),
			attribute.String("k8s.resource.namespace", note.Namespace),
		),
	)
	err := s.k8s.Create(ctx, note)
	rk = classifyK8sErr(err)
	s.metrics.k8sCreate.WithLabelValues(string(rk)).Inc()
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()

	if err != nil {
		logger.Error("create note", slog.String("result", string(rk)), slog.Any("error", err))
		http.Error(w,
			fmt.Sprintf(`{"error":%q,"result":%q}`, err.Error(), rk),
			rk.httpStatus(),
		)
		return
	}

	logger.Info("created note",
		slog.String("name", note.Name),
		slog.String("namespace", note.Namespace),
		slog.String("uid", string(note.UID)),
	)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":    "created",
		"name":      note.Name,
		"namespace": note.Namespace,
		"uid":       note.UID,
	})
}

// --- error classification ---

func classifyK8sErr(err error) resultKind {
	switch {
	case err == nil:
		return resultOK
	case apierrors.IsAlreadyExists(err):
		return resultConflict
	case apierrors.IsInvalid(err):
		return resultInvalid
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		return resultForbidden
	case apierrors.IsTooManyRequests(err):
		return resultRateLimited
	case apierrors.IsTimeout(err), apierrors.IsServerTimeout(err):
		return resultTimeout
	case apierrors.IsServiceUnavailable(err):
		return resultUnavailable
	default:
		return resultUnknown
	}
}

func (r resultKind) httpStatus() int {
	switch r {
	case resultOK:
		return http.StatusCreated
	case resultBadRequest:
		return http.StatusBadRequest
	case resultConflict:
		return http.StatusConflict
	case resultInvalid:
		return http.StatusUnprocessableEntity
	case resultForbidden:
		return http.StatusForbidden
	case resultRateLimited:
		return http.StatusTooManyRequests
	case resultTimeout:
		return http.StatusGatewayTimeout
	case resultUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// --- request id + slog plumbing ---

func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set("X-Request-ID", id)
		ctx := context.WithValue(r.Context(), requestIDKey, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func requestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey).(string); ok {
		return v
	}
	return ""
}

func loggerFromContext(ctx context.Context) *slog.Logger {
	return slog.Default().With(
		slog.String("request_id", requestIDFromContext(ctx)),
	)
}

// --- metrics ---

type metrics struct {
	registry         *prometheus.Registry
	webhooksReceived *prometheus.CounterVec
	webhookDuration  *prometheus.HistogramVec
	k8sCreate        *prometheus.CounterVec
}

func newMetrics() *metrics {
	r := prometheus.NewRegistry()
	r.MustRegister(prometheus.NewGoCollector())
	r.MustRegister(prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}))

	m := &metrics{
		registry: r,
		webhooksReceived: prometheus.NewCounterVec(
			prometheus.CounterOpts{Name: "noteapi_webhooks_received_total", Help: "Total webhooks received."},
			[]string{"result"},
		),
		webhookDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{Name: "noteapi_webhook_duration_seconds", Help: "Webhook handler latency.", Buckets: prometheus.DefBuckets},
			[]string{"result"},
		),
		k8sCreate: prometheus.NewCounterVec(
			prometheus.CounterOpts{Name: "noteapi_k8s_create_total", Help: "Note CRD creation attempts."},
			[]string{"outcome"},
		),
	}
	r.MustRegister(m.webhooksReceived, m.webhookDuration, m.k8sCreate)
	return m
}

func (m *metrics) adminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

// --- otel ---

func initTracer(ctx context.Context) (func(context.Context) error, error) {
	endpoint := envOr("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4318")
	exporter, err := otlptrace.New(ctx,
		otlptracehttp.NewClient(
			otlptracehttp.WithEndpointURL(endpoint+"/v1/traces"),
			otlptracehttp.WithTimeout(5*time.Second),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("otlp trace exporter: %w", err)
	}
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName("note-api"),
			semconv.ServiceVersion(envOr("VERSION", "dev")),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("trace resource: %w", err)
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	return tp.Shutdown, nil
}

// --- helpers ---

func sanitize(s string) string {
	out := make([]rune, 0, len(s))
	dash := false
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			out = append(out, r+('a'-'A'))
			dash = false
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			out = append(out, r)
			dash = false
		default:
			if !dash && len(out) > 0 {
				out = append(out, '-')
				dash = true
			}
		}
	}
	for len(out) > 0 && out[len(out)-1] == '-' {
		out = out[:len(out)-1]
	}
	if len(out) == 0 {
		return "note"
	}
	return string(out)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
```

### `note-api/Dockerfile`

Identical to Part 1 — multi-stage build → distroless. No changes for Part 3.

### `note-api/docker-bake.hcl`

Identical to Part 1.

### `docker-compose.yaml`

(See Step 6 above — this is one file, sibling to `note-api/`.)

### `compose/otel-config.yaml`, `compose/tempo.yaml`, `compose/prometheus.yml`, `compose/grafana/datasources/datasources.yaml`

(See Steps 6a–6d above.)

---

## Recap — what got added across the series

| Concern               | Part | Where                                                          |
| --------------------- | ---- | -------------------------------------------------------------- |
| HTTP routing          | 1    | `http.NewServeMux`                                             |
| K8s connection        | 1    | `loadKubeConfig`                                               |
| CR creation (untyped) | 1    | `dynamic.Interface` + `unstructured.Unstructured`              |
| CR creation (typed)   | 2    | `client.Client` + `&notev1alpha1.Note{}`                       |
| CRD source of truth   | 2    | `note_types.go` + `make manifests`                             |
| Multi-module wiring   | 2    | `replace ../note-operator`                                     |
| Structured logging    | 3    | `log/slog` + `requestIDMiddleware`                             |
| Graceful shutdown     | 3    | `signal.NotifyContext` + `srv.Shutdown`                        |
| Error classification  | 3    | `classifyK8sErr` + `resultKind.httpStatus`                     |
| Prometheus metrics    | 3    | private registry + admin port                                  |
| OTel traces           | 3    | `otelhttp.NewHandler` + `tracer.Start`                         |
| Local stack           | 3    | `docker compose up` (api + collector + Tempo + Prom + Grafana) |

**Total Go in `main.go`:** ~450 lines. **Total config across compose + Grafana
provisioning:** ~80 lines. **External dependencies:** `controller-runtime`,
`prometheus/client_golang`, the `otel` SDK + OTLP exporter, plus the
`note-operator` types.

That's the full operational shape — the rest is the production-specific patterns
documented in this repo's `docs/design/` and `docs/adr/`.
