# 08. Dispatcher

The dispatcher is the brain. Given an HTTP request:

1. Parse `/{provider_type}/{webhook_id}` → instance lookup
2. Read the body (up to the configured limit)
3. Verify signature via the Provider
4. Compute idempotency key; check the tracker
5. Hand body to `Provider.Handle` → typed `BackendRequest`
6. Hand `BackendRequest` to `Backend.Execute` → `ExecResult`
7. Build the response shape via `Provider.BuildResponse`
8. Write the response, emit metrics, close the trace span

The whole pipeline is one method on a `*Dispatcher` so it stays
linear and easy to read. This file is the largest of the demo —
because the dispatcher *is* where the architecture pays off.

## Files in this phase

```
internal/webhook/
├── instance.go        # the resolved Instance (config + provider/backend bound)
├── idempotency.go     # in-memory TTL+LRU tracker
└── dispatcher.go      # the request pipeline
```

## Resolved instance

The dispatcher doesn't store raw HCL — it stores a fully-resolved
instance with the Provider/Backend bound and their decoded configs.

### `internal/webhook/instance.go`

```go
package webhook

import (
    "fmt"

    "github.com/example/webhookd-demo/internal/config"
)

// Instance is one fully-resolved webhook instance: the configured
// Provider+Backend pair, their decoded configs, and the operator-
// chosen webhook ID (the routing key).
type Instance struct {
    ID             string
    Provider       Provider
    ProviderConfig ProviderConfig
    Backend        Backend
    BackendConfig  BackendConfig
}

// Resolve walks the parsed config and produces a slice of Instances,
// each with its Provider/Backend bound from the registry.
func Resolve(cfg *config.File, reg *Registry) ([]Instance, error) {
    out := make([]Instance, 0, len(cfg.Instances))
    for _, ib := range cfg.Instances {
        p, ok := reg.Provider(ib.Provider.Type)
        if !ok {
            return nil, fmt.Errorf("instance %q: unknown provider type %q",
                ib.ID, ib.Provider.Type)
        }
        pcfg, diags := p.DecodeConfig(ib.Provider.Body, nil)
        if diags.HasErrors() {
            return nil, fmt.Errorf("instance %q: provider config: %s",
                ib.ID, diags.Error())
        }

        b, ok := reg.Backend(ib.Backend.Type)
        if !ok {
            return nil, fmt.Errorf("instance %q: unknown backend type %q",
                ib.ID, ib.Backend.Type)
        }
        bcfg, diags := b.DecodeConfig(ib.Backend.Body, nil)
        if diags.HasErrors() {
            return nil, fmt.Errorf("instance %q: backend config: %s",
                ib.ID, diags.Error())
        }

        out = append(out, Instance{
            ID:             ib.ID,
            Provider:       p,
            ProviderConfig: pcfg,
            Backend:        b,
            BackendConfig:  bcfg,
        })
    }
    return out, nil
}
```

## Idempotency tracker

In-memory, per-instance, TTL + LRU bounded. Production-grade
deployments would back this with Redis or a database; the demo doesn't
need horizontal scale for the tracker.

### `internal/webhook/idempotency.go`

```go
package webhook

import (
    "container/list"
    "sync"
    "time"
)

// IdempotencyTracker is an in-memory dedupe cache keyed by
// (instance_id, provider_key). Entries expire after TTL or are
// evicted when MaxEntries is reached.
type IdempotencyTracker struct {
    mu         sync.Mutex
    ttl        time.Duration
    maxEntries int
    entries    map[string]*list.Element
    order      *list.List // most-recent at front
    now        func() time.Time
}

type idemEntry struct {
    key       string
    expiresAt time.Time
}

// NewIdempotencyTracker constructs a tracker with the given TTL and
// max entries. now defaults to time.Now when nil.
func NewIdempotencyTracker(ttl time.Duration, maxEntries int) *IdempotencyTracker {
    if maxEntries <= 0 {
        maxEntries = 10_000
    }
    return &IdempotencyTracker{
        ttl:        ttl,
        maxEntries: maxEntries,
        entries:    make(map[string]*list.Element, maxEntries),
        order:      list.New(),
        now:        time.Now,
    }
}

// Acquire attempts to claim the key. Returns true if claimed
// (caller proceeds), false if a previous claim is still live (caller
// should treat as a duplicate).
//
// On success, the caller is expected to release via Release once the
// request completes — though the entry will also expire on TTL.
func (t *IdempotencyTracker) Acquire(instanceID, key string) bool {
    if key == "" {
        return true // no key, no dedupe
    }
    full := compositeKey(instanceID, key)

    t.mu.Lock()
    defer t.mu.Unlock()

    now := t.now()

    if e, ok := t.entries[full]; ok {
        ent := e.Value.(*idemEntry)
        if now.Before(ent.expiresAt) {
            return false // still claimed
        }
        // expired — fall through and refresh
        t.removeLocked(e)
    }

    // evict if at capacity
    for t.order.Len() >= t.maxEntries {
        oldest := t.order.Back()
        if oldest == nil {
            break
        }
        t.removeLocked(oldest)
    }

    ent := &idemEntry{key: full, expiresAt: now.Add(t.ttl)}
    e := t.order.PushFront(ent)
    t.entries[full] = e
    return true
}

// Release frees the key early (before TTL). Calling Release on an
// unknown key is a no-op.
func (t *IdempotencyTracker) Release(instanceID, key string) {
    if key == "" {
        return
    }
    full := compositeKey(instanceID, key)
    t.mu.Lock()
    defer t.mu.Unlock()
    if e, ok := t.entries[full]; ok {
        t.removeLocked(e)
    }
}

func (t *IdempotencyTracker) removeLocked(e *list.Element) {
    ent := e.Value.(*idemEntry)
    t.order.Remove(e)
    delete(t.entries, ent.key)
}

func compositeKey(instance, k string) string {
    return instance + "|" + k
}
```

A few choices worth flagging:

- **`Acquire` returns false when the key is *still claimed*.** The
  dispatcher treats a `false` return as a 200/no-op response — the
  prior request is still running or just finished, so retrying is
  redundant. Production code may want to distinguish "currently
  running" from "already completed within TTL"; the demo doesn't.
- **No background sweeper.** Expired entries get reaped lazily on
  next Acquire of the same key. Keeps the API minimal; small
  memory penalty on idle keys.
- **`now` is injectable** for test determinism — table tests can pass
  a clock function.

## The dispatcher

The pipeline. Long-ish — kept in one method intentionally so the flow
is legible top-to-bottom.

### `internal/webhook/dispatcher.go`

```go
package webhook

import (
    "context"
    "encoding/json"
    "errors"
    "io"
    "log/slog"
    "net/http"
    "strings"
    "time"

    "go.opentelemetry.io/otel"
    "go.opentelemetry.io/otel/attribute"
    "go.opentelemetry.io/otel/trace"

    "github.com/example/webhookd-demo/internal/httpx"
    "github.com/example/webhookd-demo/internal/observability"
)

// DispatcherConfig is what the dispatcher needs at construction.
type DispatcherConfig struct {
    Instances    []Instance
    Tracker      *IdempotencyTracker
    Metrics      *observability.Metrics
    MaxBodyBytes int64
    Logger       *slog.Logger
}

// Dispatcher routes incoming requests and orchestrates the
// Provider → Backend pipeline.
type Dispatcher struct {
    instances map[string]Instance // keyed by webhook_id
    tracker   *IdempotencyTracker
    metrics   *observability.Metrics
    maxBody   int64
    log       *slog.Logger
    tracer    trace.Tracer
}

// NewDispatcher builds a Dispatcher. Returns an error if two instances
// share the same webhook_id.
func NewDispatcher(cfg DispatcherConfig) (*Dispatcher, error) {
    instances := make(map[string]Instance, len(cfg.Instances))
    for _, inst := range cfg.Instances {
        if _, dup := instances[inst.ID]; dup {
            return nil, errors.New("duplicate webhook id: " + inst.ID)
        }
        instances[inst.ID] = inst
    }
    return &Dispatcher{
        instances: instances,
        tracker:   cfg.Tracker,
        metrics:   cfg.Metrics,
        maxBody:   cfg.MaxBodyBytes,
        log:       cfg.Logger,
        tracer:    otel.Tracer("webhookd-demo/dispatcher"),
    }, nil
}

// Handler returns the http.Handler the public mux mounts under
// /{provider_type}/{webhook_id}.
func (d *Dispatcher) Handler() http.Handler {
    mux := http.NewServeMux()
    mux.Handle("POST /{provider}/{webhook_id}", http.HandlerFunc(d.serve))
    return mux
}

// serve is the request pipeline.
func (d *Dispatcher) serve(w http.ResponseWriter, r *http.Request) {
    ctx, span := d.tracer.Start(r.Context(), "dispatcher.serve",
        trace.WithSpanKind(trace.SpanKindServer),
    )
    defer span.End()

    providerType := r.PathValue("provider")
    webhookID := r.PathValue("webhook_id")

    span.SetAttributes(
        attribute.String("webhook.provider_type", providerType),
        attribute.String("webhook.instance_id", webhookID),
    )

    inst, ok := d.instances[webhookID]
    if !ok {
        d.respondError(ctx, w, providerType, webhookID, http.StatusNotFound,
            "UnknownInstance", "no instance configured for webhook_id")
        return
    }
    if inst.Provider.Type() != providerType {
        d.respondError(ctx, w, providerType, webhookID, http.StatusNotFound,
            "ProviderMismatch", "provider_type does not match configured instance")
        return
    }

    // 1. Read body.
    body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, d.maxBody))
    if err != nil {
        d.respondError(ctx, w, providerType, webhookID, http.StatusRequestEntityTooLarge,
            "BodyTooLarge", err.Error())
        return
    }

    // 2. Verify signature.
    if err := inst.Provider.VerifySignature(r, body, inst.ProviderConfig); err != nil {
        d.metrics.SignatureFailures.WithLabelValues(providerType).Inc()
        d.respondError(ctx, w, providerType, webhookID, http.StatusUnauthorized,
            "InvalidSignature", err.Error())
        return
    }

    // 3. Idempotency.
    idemKey, err := inst.Provider.IdempotencyKey(r, body)
    if err != nil {
        // Failing to compute the key is a payload error, not 5xx.
        d.respondError(ctx, w, providerType, webhookID, http.StatusBadRequest,
            "IdempotencyKeyFailed", err.Error())
        return
    }
    if !d.tracker.Acquire(inst.ID, idemKey) {
        d.metrics.IdempotencyHits.WithLabelValues(inst.ID).Inc()
        d.respondNoOp(ctx, w, inst, "DuplicateRequest", "idempotent retry")
        return
    }
    // We don't Release on success — the entry expires on TTL so
    // genuine retries within the window stay deduped.

    // 4. Provider.Handle: bytes → BackendRequest.
    started := time.Now()
    breq, err := inst.Provider.Handle(ctx, body, inst.ProviderConfig)
    if err != nil {
        d.tracker.Release(inst.ID, idemKey) // free the key on failure
        d.respondHandleErr(ctx, w, inst, err)
        return
    }

    // 5. Backend.Execute: BackendRequest → ExecResult.
    backendCtx, backendSpan := d.tracer.Start(ctx, "backend.execute",
        trace.WithAttributes(
            attribute.String("backend.type", inst.Backend.Type()),
            attribute.String("backend.request_kind", breq.Kind()),
        ),
    )
    res := inst.Backend.Execute(backendCtx, breq, inst.BackendConfig)
    backendSpan.SetAttributes(
        attribute.Int("backend.result_kind", int(res.Kind)),
        attribute.Int("backend.http_status", res.HTTPStatus),
    )
    backendSpan.End()

    duration := time.Since(started)
    d.metrics.DispatchDuration.WithLabelValues(inst.ID).Observe(duration.Seconds())
    d.metrics.DispatchTotal.WithLabelValues(inst.ID, kindString(res.Kind)).Inc()
    d.metrics.BackendApplyTotal.WithLabelValues(inst.Backend.Type(), kindString(res.Kind)).Inc()
    d.metrics.BackendSyncDuration.WithLabelValues(inst.Backend.Type(), kindString(res.Kind)).Observe(duration.Seconds())

    if res.Kind != webhookSuccess(res) {
        d.tracker.Release(inst.ID, idemKey) // free the key on non-success too
    }

    // 6. Build response and write.
    d.respond(ctx, w, inst, res)

    span.SetStatus(spanStatus(res.Kind))
}

// respondError emits a typed error response without going through the
// Provider — used for the framing failures (unknown instance, body
// too large, etc.) before the Provider has been engaged.
func (d *Dispatcher) respondError(ctx context.Context, w http.ResponseWriter, providerType, webhookID string, status int, reason, detail string) {
    body := map[string]string{
        "status": "error",
        "reason": reason,
    }
    if detail != "" {
        body["detail"] = detail
    }
    if rid := httpx.RequestID(ctx); rid != "" {
        body["request_id"] = rid
    }
    if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
        body["trace_id"] = sc.TraceID().String()
    }
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(status)
    _ = json.NewEncoder(w).Encode(body)

    d.metrics.HTTPRequestsTotal.WithLabelValues(providerType, "POST", strings.TrimSpace(http.StatusText(status))).Inc()
    d.log.WarnContext(ctx, "dispatch error",
        slog.String("instance", webhookID),
        slog.String("provider", providerType),
        slog.Int("status", status),
        slog.String("reason", reason),
        slog.String("detail", detail),
    )
}

// respondNoOp returns 200 with a "no-op" body — used for idempotency
// hits and trigger-status mismatches (we don't want Jira to retry
// these).
func (d *Dispatcher) respondNoOp(ctx context.Context, w http.ResponseWriter, inst Instance, reason, detail string) {
    res := ExecResult{
        Kind:       ResultNoOp,
        HTTPStatus: http.StatusOK,
        Reason:     reason,
        Detail:     detail,
    }
    d.respond(ctx, w, inst, res)
}

// respondHandleErr maps Provider.Handle errors to the right HTTP
// status. Provider-specific errors should be `errors.Is`-checkable.
func (d *Dispatcher) respondHandleErr(ctx context.Context, w http.ResponseWriter, inst Instance, err error) {
    // We can't import the provider package without a cycle, so we
    // pattern-match on the error string for the demo. Production
    // code can either centralize the typed errors in `webhook/`
    // or use a small ProviderError interface.
    msg := err.Error()
    switch {
    case strings.Contains(msg, "trigger status mismatch"):
        d.respondNoOp(ctx, w, inst, "TriggerStatusMismatch", msg)
    case strings.Contains(msg, "missing required field"):
        d.respond(ctx, w, inst, ExecResult{
            Kind:       ResultClientError,
            HTTPStatus: http.StatusUnprocessableEntity,
            Reason:     "MissingField",
            Detail:     msg,
        })
    default:
        d.respond(ctx, w, inst, ExecResult{
            Kind:       ResultServerError,
            HTTPStatus: http.StatusInternalServerError,
            Reason:     "HandleFailed",
            Detail:     msg,
        })
    }
}

// respond is the canonical exit point: shape via Provider.BuildResponse
// + write JSON.
func (d *Dispatcher) respond(ctx context.Context, w http.ResponseWriter, inst Instance, res ExecResult) {
    var traceID string
    if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
        traceID = sc.TraceID().String()
    }
    requestID := httpx.RequestID(ctx)

    body := inst.Provider.BuildResponse(res, traceID, requestID)
    status := res.HTTPStatus
    if status == 0 {
        status = statusFromKind(res.Kind)
    }

    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(status)
    _ = json.NewEncoder(w).Encode(body)

    d.metrics.HTTPRequestsTotal.WithLabelValues(
        inst.Provider.Type(), "POST", strings.TrimSpace(http.StatusText(status)),
    ).Inc()
}

// kindString maps a ResultKind to its metric-label string.
func kindString(k ResultKind) string {
    switch k {
    case ResultSuccess:
        return "success"
    case ResultNoOp:
        return "noop"
    case ResultClientError:
        return "client_error"
    case ResultServerError:
        return "server_error"
    case ResultTimeout:
        return "timeout"
    default:
        return "unknown"
    }
}

func statusFromKind(k ResultKind) int {
    switch k {
    case ResultSuccess, ResultNoOp:
        return http.StatusOK
    case ResultClientError:
        return http.StatusUnprocessableEntity
    case ResultServerError:
        return http.StatusInternalServerError
    case ResultTimeout:
        return http.StatusGatewayTimeout
    default:
        return http.StatusInternalServerError
    }
}

func spanStatus(k ResultKind) (codes.Code, string) {
    switch k {
    case ResultSuccess, ResultNoOp:
        return codes.Ok, ""
    default:
        return codes.Error, kindString(k)
    }
}

// webhookSuccess returns the success kind so the dispatcher's
// idempotency-release logic reads cleanly. Existence is a stylistic
// choice — feel free to inline.
func webhookSuccess(_ ExecResult) ResultKind { return ResultSuccess }
```

> **Note on `codes`:** Add `import "go.opentelemetry.io/otel/codes"`
> at the top of the file. The `spanStatus` function uses it; the
> snippet above intentionally elides the import line for brevity.

## Why is `respondHandleErr` doing string-matching?

The dispatcher and the JSM provider are in different packages.
Importing `jsm` from `webhook` would create a cycle. Three options:

- **A) String-match on error messages** *(what the demo does)*. Cheap;
  brittle if provider error messages change.
- **B) Provider exposes a `ProviderError` interface.** The provider's
  errors implement a method like `HTTPStatus() int` and `Reason() string`.
  Dispatcher type-asserts. Cleaner; takes a small amount of plumbing.
- **C) Provider's `Handle` returns `(BackendRequest, ExecResult, error)`.**
  Provider can pre-shape its own ExecResult on errors that have
  Provider-specific semantics (trigger mismatch, missing field).
  Most type-safe; loosens the contract slightly.

Production webhookd uses (B) or (C). The demo uses (A) for brevity —
but it's exactly the kind of thing you'd refactor before merging the
real change.

## What we proved

- [x] Multi-tenant routing via path values — one `ServeMux` line
- [x] Each request: signature → idempotency → Provider → Backend → response
- [x] OTel spans for `dispatcher.serve` + `backend.execute`
- [x] Metrics emitted for every code path
- [x] Idempotency tracker is bounded, TTL'd, and isolated per instance

Next: [09-main.md](09-main.md) — wire it all together.
