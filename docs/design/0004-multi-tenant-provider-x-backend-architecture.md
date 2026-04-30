---
id: DESIGN-0004
title: "Multi-Tenant Provider x Backend Architecture"
status: Draft
author: Donald Gifford
created: 2026-04-30
---
<!-- markdownlint-disable-file MD025 MD041 -->

# DESIGN 0004: Multi-Tenant Provider x Backend Architecture

**Status:** Draft
**Author:** Donald Gifford
**Date:** 2026-04-30

<!--toc:start-->
- [Overview](#overview)
- [Goals and Non-Goals](#goals-and-non-goals)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Background](#background)
- [Detailed Design](#detailed-design)
  - [Package layout](#package-layout)
  - [Provider interface](#provider-interface)
  - [Backend interface](#backend-interface)
  - [AsyncBackend interface (Phase 4)](#asyncbackend-interface-phase-4)
    - [202-Accepted response body (wire contract)](#202-accepted-response-body-wire-contract)
  - [BackendRequest](#backendrequest)
  - [ExecResult and ResultKind](#execresult-and-resultkind)
  - [Registry](#registry)
  - [Instance and InstanceMap](#instance-and-instancemap)
  - [Dispatcher](#dispatcher)
  - [Idempotency tracker](#idempotency-tracker)
  - [Routing](#routing)
- [API / Interface Changes](#api--interface-changes)
  - [HTTP API](#http-api)
  - [CLI changes](#cli-changes)
- [Data Model](#data-model)
  - [Configuration HCL2 schema](#configuration-hcl2-schema)
  - [Validation: gohcl decoding (no separate JSON Schema)](#validation-gohcl-decoding-no-separate-json-schema)
  - [In-memory Config struct](#in-memory-config-struct)
  - [K8s-side annotations carrying forward](#k8s-side-annotations-carrying-forward)
- [Error Handling](#error-handling)
  - [Error classification at each layer](#error-classification-at-each-layer)
  - [HTTP status code mapping](#http-status-code-mapping)
  - [Logging and tracing](#logging-and-tracing)
- [Testing Strategy](#testing-strategy)
  - [Unit](#unit)
  - [Per-integration](#per-integration)
  - [Dispatcher and registry](#dispatcher-and-registry)
  - [End-to-end (envtest + kind)](#end-to-end-envtest--kind)
  - [The Phase 3 abstraction-pressure test](#the-phase-3-abstraction-pressure-test)
- [Migration / Rollout Plan](#migration--rollout-plan)
- [Resolved Decisions](#resolved-decisions)
- [References](#references)
  - [Implements](#implements)
  - [Binding decisions](#binding-decisions)
  - [Background](#background-1)
  - [Superseded / built on](#superseded--built-on)
<!--toc:end-->

## Overview

This DESIGN implements RFC-0001's multi-tenant Provider × Backend architecture. It defines the concrete interface signatures, package layout, HCL2 configuration schema (per ADR-0009), dispatcher contract, and idempotency mechanism that the refactor needs. Behavior visible to the existing JSM workflow is preserved — the synchronous response contract (ADR-0006), HMAC verification, observability stack, and Helm-chart shape carry forward unchanged. New: open `Backend` / `BackendRequest` interfaces replace the closed `Action` union, multi-tenant routing on `/{provider_type}/{webhook_id}`, HCL-driven instance configuration with typed `gohcl` decoding, and per-provider idempotency keys.

## Goals and Non-Goals

### Goals

- Define **concrete Go interface signatures** for `Provider`, `Backend`, `AsyncBackend`, `BackendRequest`, and `ExecResult` such that adding a new integration is one new package + one import line + one `init()` registration, with **zero edits inside `internal/webhook/`**.
- Define the **HCL2 configuration schema** for multi-tenant instances (per ADR-0009), including the per-integration typed-decoding pattern via `gohcl.DecodeBody` and the in-memory `Config` struct it decodes into.
- Define the **dispatcher contract**: how a request flows from HTTP entry through provider lookup, signature verification, idempotency, provider parsing, backend execution, and response shaping.
- Define the **idempotency mechanism** (pod-local `sync.Map` with TTL eviction) including memory bounds and eviction policy.
- Define the **migration path** from the current single-tenant env-var shape: hard cutover at the version that lands Phase 2.
- Preserve every behavior of the current JSM → SAMLGroupMapping flow; the JSM-side automation rule observes no contract changes.

### Non-Goals

- **Implementation code.** This is a design doc, not a PR. Implementation lands across IMPL-0004's phases.
- **Hot-reload of configuration.** Configuration is read at startup. Hot-reload defers to a follow-up RFC.
- **Durable queue / state-management infrastructure.** Per ADR-0008, deferred to Phase 5; this design provides the seam (`AsyncBackend`) but no queue implementation.
- **Defining the second integration's specifics.** Phase 3's Backend (AWS or GitHub) is its own DESIGN doc.
- **Hot-replacing integrations at runtime.** Per ADR-0010, integrations are static build-time imports.
- **Cross-pod idempotency coordination.** Pod-local is intentional; the few cross-pod duplicate cases that L7 affinity doesn't catch produce the same outcome as today (duplicate work, idempotent at the K8s level via SSA).

## Background

This DESIGN descends from RFC-0001, which descends from INV-0001 (architecture review) and INV-0002 (Argo Events alternative, rejected). The investigations resolved the major shape decisions; the RFC ratified them and split out three ADRs (0008/0009/0010) for the most independently-citable ones; this DESIGN turns that shape into concrete signatures.

The existing surface this design must respect:

- **DESIGN-0001 + IMPL-0001** — HTTP framework, signing, observability, rate limiting. Survives unchanged.
- **DESIGN-0002 + IMPL-0002** — JSM webhook → SAMLGroupMapping CR provisioning. The current JSM Provider + K8s Backend pair becomes the reference implementation under the new package layout. Status flips to *Implemented-but-superseded* when this DESIGN's Phase 2 lands.
- **DESIGN-0003 + IMPL-0003** — Helm chart and release pipeline. Phase 2 of this design requires chart updates to render the HCL config into a `ConfigMap`; the existing `existingSecret` pattern for env-mapped secrets carries forward.
- **ADR-0006** — Synchronous response contract. *Foundational; the entire dispatcher contract preserves this.*
- **ADRs 0004 / 0005 / 0007** — K8s typed client, SSA, trace-context propagation. Constraints on the K8s Backend implementation in Phase 0+.

## Detailed Design

### Package layout

```
internal/
├── webhook/
│   ├── dispatcher.go          # routing + plumbing only — no integration logic
│   ├── provider.go            # Provider interface
│   ├── backend.go             # Backend, AsyncBackend interfaces
│   ├── request.go             # BackendRequest interface
│   ├── result.go              # ExecResult, ResultKind, HTTPStatus mapping
│   ├── registry.go            # global registry + Registry type for tests
│   ├── instance.go            # Instance type, InstanceMap with (provider, id) lookup
│   ├── idempotency.go         # sync.Map-backed in-flight tracker with TTL
│   └── errors.go              # webhook.ErrBadRequest, ErrUnprocessable sentinels
├── integrations/
│   ├── jsm/                   # Provider only (input)
│   │   ├── init.go            # webhook.RegisterProvider
│   │   ├── provider.go
│   │   ├── payload.go
│   │   ├── extract.go
│   │   ├── signature.go
│   │   └── response.go
│   ├── k8s/                   # Backend only (output)
│   │   ├── init.go            # webhook.RegisterBackend
│   │   ├── backend.go         # what was internal/webhook/executor.go
│   │   ├── apply.go
│   │   ├── watch.go
│   │   ├── classify.go
│   │   └── wizapi/            # what was internal/webhook/wizapi/
│   ├── github/                # Provider AND Backend — distinct types in one package
│   │   ├── init.go
│   │   ├── provider.go        # type Provider
│   │   ├── backend.go         # type Backend
│   │   └── client.go          # shared auth/HTTP client
│   ├── aws/                   # Backend only (output)
│   └── http/                  # Backend only (output) — generic forwarder
│   └── (each integration ships a config.go with its hcl-tagged Config struct)
└── config/
    ├── config.go              # HCL2 loader: parses file or directory
    ├── schema.go              # top-level HCL schema (defaults, runtime, instance "..." {})
    └── instances.go           # walks parsed body, dispatches per-block to integration decoders
```

### Provider interface

```go
// internal/webhook/provider.go

package webhook

import (
    "context"
    "net/http"
)

// Provider is the input seam — one implementation per vendor that sends
// webhooks to webhookd. Implementations must be goroutine-safe (the
// dispatcher concurrently invokes VerifySignature, IdempotencyKey, and
// Handle from many request goroutines) and Handle must be pure: no I/O
// against Kubernetes, the network, or any other side-effectful system.
type Provider interface {
    // Type returns the URL path segment that routes to this provider
    // (e.g. "jsm", "github"). Stable; used as a metrics label and a
    // routing key, so renaming is a breaking change.
    Type() string

    // VerifySignature validates request authenticity using this provider's
    // own conventions (header names, canonical body shape, HMAC algorithm).
    // Must be timing-safe (use hmac.Equal or equivalent). Returns nil on
    // success; any error means the dispatcher responds 401.
    //
    // body is the already-bounded request body. cfg is the per-instance
    // ProviderConfig produced by the Provider's own config decoder.
    VerifySignature(r *http.Request, body []byte, cfg ProviderConfig) error

    // IdempotencyKey returns a per-event dedup key for this payload — for
    // example, a JSM ticket key, GitHub X-GitHub-Delivery header, or Slack
    // event_id. Empty string disables idempotency for this request (use
    // when the provider has no natural per-event ID).
    //
    // Errors here are treated as malformed payload and surface as 400.
    IdempotencyKey(r *http.Request, body []byte) (string, error)

    // Handle decodes the verified body and decides what work to do. Returns:
    //   - non-nil BackendRequest: the executor will run req via the bound Backend.
    //   - non-nil error wrapping webhook.ErrBadRequest: malformed payload (400).
    //   - non-nil error wrapping webhook.ErrUnprocessable: semantic error (422).
    //   - non-nil error otherwise: internal error (500).
    //
    // For "received but no work needed" (e.g. JSM ticket in non-trigger status),
    // return a BackendRequest whose BackendType() is "noop" — the dispatcher
    // routes it to the no-op backend, which always returns ResultNoop. This is
    // cleaner than a separate (Action, error) signal.
    Handle(ctx context.Context, body []byte, cfg ProviderConfig) (BackendRequest, error)

    // BuildResponse shapes an ExecResult into a per-provider response body
    // (e.g. JSM cares about crName + traceId for ticket-comment automation).
    // The dispatcher writes status code from ExecResult.Kind.HTTPStatus()
    // and the body from BuildResponse.
    BuildResponse(res ExecResult, traceID, requestID string) any

    // DecodeConfig decodes an HCL block body into the Provider's typed
    // config. Implementations call gohcl.DecodeBody with their own struct
    // and return it as a ProviderConfig (an empty-interface alias). The
    // returned value is what Handle / VerifySignature / IdempotencyKey
    // receive back at request time.
    //
    // evalCtx is the HCL evaluation context — typically just exposes the
    // env() function for ${env("VAR")}-style references and any built-ins
    // we want to permit. Per-provider validation beyond what `hcl:""` tags
    // express lives here.
    DecodeConfig(body hcl.Body, evalCtx *hcl.EvalContext) (ProviderConfig, hcl.Diagnostics)
}

// ProviderConfig is opaque to the dispatcher; each Provider implementation
// defines and consumes its own concrete struct (with `hcl:""` tags). The
// empty interface is acceptable here because the dispatcher never
// introspects ProviderConfig values, only stores and returns them.
type ProviderConfig interface{}
```

### Backend interface

```go
// internal/webhook/backend.go

package webhook

import "context"

// Backend is the output seam — one implementation per downstream system
// webhookd dispatches to. Implementations must be goroutine-safe.
//
// Backends own all side-effecting work: K8s SSA, HTTP POSTs, AWS API
// calls, etc. Cross-cutting concerns — span creation, retry classification,
// metric observation — also live in the backend, *not* in the dispatcher,
// so each backend can shape its own observability.
type Backend interface {
    // Type returns the backend identifier (e.g. "k8s", "aws", "http",
    // "noop"). Stable; used as a metrics label and config key.
    Type() string

    // Execute performs the work for req synchronously. The returned
    // ExecResult carries everything the dispatcher needs to write the
    // HTTP response: kind (status code), reason (response body), optional
    // resource identity (CRName, Namespace), optional ObservedGeneration.
    //
    // Backends MUST NOT panic on bad inputs — return ExecResult{Kind:
    // ResultBadRequest} or {ResultInternalError} instead. The dispatcher
    // recovers from panics as a safety net, but relying on that loses the
    // structured response body.
    Execute(ctx context.Context, req BackendRequest, cfg BackendConfig) ExecResult

    // DecodeConfig decodes an HCL block body into the Backend's typed
    // config. Same shape as Provider.DecodeConfig — call gohcl.DecodeBody
    // against your own struct and return it.
    DecodeConfig(body hcl.Body, evalCtx *hcl.EvalContext) (BackendConfig, hcl.Diagnostics)
}

// BackendConfig is opaque to the dispatcher; each Backend defines its own
// concrete struct (with `hcl:""` tags). Same shape as ProviderConfig.
type BackendConfig interface{}
```

### AsyncBackend interface (Phase 4)

```go
// AsyncBackend opts into the long-running-work shape from ADR-0008.
// Backends that need more than the HTTP-timeout budget implement this
// alongside Backend; the synchronous Execute remains the fallback for
// when the work *does* fit.
type AsyncBackend interface {
    Backend

    // ExecuteAsync starts the work in a goroutine and returns a token
    // the dispatcher uses to track it. The dispatcher responds 202
    // Accepted to the original webhook caller with the token in the body.
    //
    // When the work completes, the dispatcher invokes the matching
    // Provider's CallbackPoster (defined in DESIGN-0005, the AsyncBackend
    // detail design) which POSTs the result to the originator's callback
    // URL with an HMAC-signed body. Pod-crash recovery: provider-side
    // callback idempotency by default, optional `webhookd.io/callback-fired-at`
    // annotation as a second-level dedupe; both per ADR-0008.
    ExecuteAsync(ctx context.Context, req BackendRequest, cfg BackendConfig) (PendingToken, error)

    // Callback runs in the dispatcher's goroutine when the work tracked
    // by token completes. Returns the final ExecResult; the dispatcher
    // hands it to the Provider's response shaper.
    Callback(ctx context.Context, token PendingToken) ExecResult
}

// PendingToken is opaque between AsyncBackend.ExecuteAsync and AsyncBackend.Callback.
// Each backend defines its own concrete type.
type PendingToken interface{}
```

The full async/callback wiring (token persistence between `ExecuteAsync` and `Callback`, callback-poster contract, retry/timeout policy for the outbound POST, `callback-fired-at` annotation handling) is not part of this DESIGN — it lands in **DESIGN-0005 — Async Backend Callback Pattern**, written when Phase 4 is scheduled. This DESIGN commits to the interface shape *and the 202-Accepted wire contract* (below) so Phase 0–3 don't paint into a corner.

#### 202-Accepted response body (wire contract)

When the dispatcher routes a request to an `AsyncBackend`, the immediate HTTP response is `202 Accepted` with a body the originating provider can correlate against the eventual callback POST. The body is provider-shaped via the same `Provider.BuildResponse` path as synchronous responses, but with a sentinel `ResultKind`:

```go
const (
    // ... existing kinds ...
    ResultPending  // 202 Accepted; AsyncBackend in flight, callback to follow
)
```

`ExecResult.Kind = ResultPending` carries enough state for the response shaper to produce a correlation token in the body:

```go
// In webhook.ExecResult, populated for ResultPending:
type ExecResult struct {
    // ...
    PendingToken string  // serialized PendingToken; opaque to dispatcher;
                         // the matching AsyncBackend recovers state from it
                         // when Callback is invoked.
    CallbackURL  string  // where webhookd will POST the final result
                         // (derived from per-instance config + ProviderConfig)
}
```

The default JSON body shape (overridable per-Provider via `BuildResponse`):

```json
{
  "status": "pending",
  "correlation_token": "<opaque base64>",
  "callback_url": "https://webhookd.example/callback/<provider_type>/<webhook_id>",
  "trace_id": "<32-hex>",
  "request_id": "<ulid>"
}
```

The full lifecycle (how `PendingToken` round-trips through whatever durable-or-not state is needed, callback-poster HMAC signing, retry/timeout policy for the outbound POST) lands in DESIGN-0005. This DESIGN commits to the wire-level token+URL+status fields so dispatcher and Provider response shapers don't need a second breaking change later.

### BackendRequest

```go
// internal/webhook/request.go

// BackendRequest is what flows from Provider.Handle to Backend.Execute.
// Each (Provider, Backend) pair agrees on a concrete type at wiring time;
// the dispatcher just plumbs it through.
//
// Concrete types live in the integration packages — e.g.
// integrations/k8s.ApplyCRRequest, integrations/aws.PublishSNSRequest.
// The dispatcher MUST NOT type-assert on them; backends do.
type BackendRequest interface {
    // BackendType returns the matching Backend.Type() this request
    // requires. The dispatcher validates that the bound backend's Type()
    // matches before invoking Execute; mismatch is a programming error
    // (the wiring is wrong) and produces an InternalError result.
    BackendType() string
}
```

The closed `Action` interface in `internal/webhook/action.go` is deleted in Phase 1. `NoopAction` becomes a `BackendRequest` whose `BackendType()` returns `"noop"`, dispatched to a built-in `noopBackend` that always returns `ResultNoop`. `ApplySAMLGroupMapping` becomes `integrations/k8s.ApplyCRRequest`.

### ExecResult and ResultKind

`ExecResult` and `ResultKind` move from `internal/webhook/result.go` essentially unchanged. ResultKind values stay stable (the existing values are good); HTTPStatus mapping stays stable.

```go
// Existing — moves into internal/webhook/result.go
type ResultKind int

const (
    ResultNoop ResultKind = iota
    ResultReady
    ResultTransientFailure
    ResultBadRequest
    ResultUnprocessable
    ResultInternalError
    ResultTimeout
)

// ExecResult is generalized away from the JSM-specific naming:
type ExecResult struct {
    Kind   ResultKind
    Reason string

    // Resource identity (optional) — populated by backends that act on
    // named resources. The K8s backend fills CRName + Namespace; an HTTP
    // backend might fill RemoteURL; an AWS backend might fill ARN.
    // Provider.BuildResponse decides which fields end up in the response body.
    ResourceID   string  // generic; replaces CRName
    ResourceKind string  // generic; replaces the implicit "SAMLGroupMapping"
    Namespace    string  // K8s-specific but harmless when empty

    // ObservedGeneration is the operator's last-seen spec generation.
    // K8s-specific; empty for non-K8s backends.
    ObservedGeneration int64

    // Backend may attach extra context for the response shaper. Each
    // Provider knows what to do with the values its paired Backend produces.
    Extra map[string]string
}
```

The renaming `CRName → ResourceID, ResourceKind` is the only material change. JSM's `BuildResponse` reads `ResourceID` and emits it as `crName` in its response body — backwards-compat at the wire layer.

### Registry

```go
// internal/webhook/registry.go

package webhook

// Registry holds the registered Provider and Backend implementations.
// The package-level globalRegistry is populated by integration packages'
// init() functions per ADR-0010; tests construct isolated Registries via
// NewRegistry.
type Registry struct {
    providers map[string]Provider
    backends  map[string]Backend
}

func NewRegistry() *Registry {
    return &Registry{
        providers: make(map[string]Provider),
        backends:  make(map[string]Backend),
    }
}

// RegisterProvider adds p to this registry. Duplicate registration of the
// same Type() panics — preferable to silent overwrite.
func (r *Registry) RegisterProvider(p Provider) { /* ... */ }
func (r *Registry) RegisterBackend(b Backend)   { /* ... */ }

func (r *Registry) LookupProvider(t string) (Provider, bool) { /* ... */ }
func (r *Registry) LookupBackend(t string) (Backend, bool)   { /* ... */ }

// Package-level convenience for the common case: integrations register
// themselves via init() into globalRegistry.
var globalRegistry = NewRegistry()

func RegisterProvider(p Provider) { globalRegistry.RegisterProvider(p) }
func RegisterBackend(b Backend)   { globalRegistry.RegisterBackend(b) }
func GlobalRegistry() *Registry    { return globalRegistry }
```

### Instance and InstanceMap

```go
// internal/webhook/instance.go

package webhook

import "time"

// Instance is the runtime tuple binding one Provider to one Backend with
// their per-instance configurations. One Instance per webhook URL.
type Instance struct {
    ID             string         // opaque, e.g. "abc123def456"
    Provider       Provider       // resolved from registry by config Type
    Backend        Backend        // resolved from registry by config Type
    ProviderConfig ProviderConfig // typed, decoded by Provider.DecodeConfig
    BackendConfig  BackendConfig  // typed, decoded by Backend.DecodeConfig
    IdempotencyTTL time.Duration  // default 5m
}

// InstanceMap is the dispatcher's lookup index — a two-key map from
// (provider_type, webhook_id) to *Instance.
type InstanceMap struct {
    byKey map[instanceKey]*Instance
}

type instanceKey struct {
    providerType string
    id           string
}

func (m *InstanceMap) Lookup(providerType, id string) (*Instance, bool) { /* ... */ }
func (m *InstanceMap) All() []*Instance                                  { /* ... */ }
```

The map is built once at startup from the HCL config and never mutated thereafter — read-only after `Build`. No locking needed for reads.

### Dispatcher

```go
// internal/webhook/dispatcher.go

type DispatcherConfig struct {
    Instances    *InstanceMap
    Logger       *slog.Logger
    Metrics      *observability.Metrics
    MaxBodyBytes int64                  // global, applies to every instance
    Idempotency  *IdempotencyTracker
}

func NewDispatcher(cfg *DispatcherConfig) *Dispatcher { /* ... */ }

func (d *Dispatcher) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    ctx := r.Context()

    // 1. Routing — extract (provider_type, webhook_id) from the URL path.
    providerType := r.PathValue("provider_type")
    id := r.PathValue("webhook_id")

    inst, ok := d.cfg.Instances.Lookup(providerType, id)
    if !ok {
        // 404 — neither path component leaks tenant info because IDs are opaque.
        http.NotFound(w, r)
        return
    }

    // 2. Body bounding.
    body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, d.cfg.MaxBodyBytes))
    if err != nil { /* 413 or 400 per existing dispatcher logic */ }

    // 3. Signature.
    if err := inst.Provider.VerifySignature(r, body, inst.ProviderConfig); err != nil {
        http.Error(w, "invalid signature", http.StatusUnauthorized)
        return
    }

    // 4. Idempotency check (per ADR's "per-provider idempotency keys").
    key, err := inst.Provider.IdempotencyKey(r, body)
    if err != nil { /* 400 — malformed payload at extraction layer */ }
    if key != "" {
        acquired, prevResult := d.cfg.Idempotency.Acquire(
            providerType, key, inst.IdempotencyTTL,
        )
        if !acquired {
            // Duplicate; respond with the cached previous result if known,
            // or 200 noop with reason "duplicate" if not.
            d.writeResponse(ctx, w, inst, prevResult)
            return
        }
        defer d.cfg.Idempotency.Release(providerType, key, /* result */)
    }

    // 5. Provider parses + decides.
    req, err := inst.Provider.Handle(ctx, body, inst.ProviderConfig)
    if err != nil {
        d.writeResponse(ctx, w, inst, classifyProviderErr(err))
        return
    }

    // 6. Validate the request type matches the bound backend.
    if req.BackendType() != inst.Backend.Type() {
        // Programming error in the integration: provider produced a request
        // its bound backend can't handle. 500.
        d.writeResponse(ctx, w, inst, ExecResult{
            Kind: ResultInternalError,
            Reason: fmt.Sprintf("provider %q produced request for backend %q, instance is bound to %q",
                inst.Provider.Type(), req.BackendType(), inst.Backend.Type()),
        })
        return
    }

    // 7. Execute.
    res := inst.Backend.Execute(ctx, req, inst.BackendConfig)

    // 8. Shape and write.
    d.writeResponse(ctx, w, inst, res)
}
```

`writeResponse` is unchanged in spirit from the current dispatcher: status code from `res.Kind.HTTPStatus()`, body from `inst.Provider.BuildResponse(res, traceID, requestID)`.

### Idempotency tracker

```go
// internal/webhook/idempotency.go

// IdempotencyTracker is a pod-local in-flight + recently-completed cache
// keyed by (provider_type, key). TTL-based eviction bounds memory; an LRU
// cap is the backstop for chatty callers.
//
// Concurrent calls for the same key block at Acquire (only the first
// returns acquired=true); subsequent callers receive the previous result
// when the work completes, or noop("duplicate") if no result is cached yet.
type IdempotencyTracker struct {
    mu      sync.Mutex
    entries map[idemKey]*idemEntry  // bounded by maxEntries
    lru     *list.List              // LRU for eviction
    cfg     IdempotencyConfig
    now     func() time.Time
}

type IdempotencyConfig struct {
    MaxEntries  int           // default 10_000 per pod
    DefaultTTL  time.Duration // default 5m
    SweepEvery  time.Duration // default 1m, expired entry GC
}

type idemKey struct {
    provider string
    key      string
}

type idemEntry struct {
    expiresAt time.Time
    result    *ExecResult  // nil = in-flight, non-nil = completed
    waiters   []chan ExecResult
}

func NewIdempotencyTracker(cfg IdempotencyConfig) *IdempotencyTracker { /* ... */ }

// Acquire returns (true, ExecResult{}) if the caller is the first for this
// key — caller proceeds with the work and MUST call Release.
// Returns (false, prevResult) if a concurrent or recent caller already did
// the work; prevResult is the cached completed result, or ExecResult{Kind:
// ResultNoop, Reason: "duplicate"} if still in-flight.
func (t *IdempotencyTracker) Acquire(provider, key string, ttl time.Duration) (bool, ExecResult) { /* ... */ }

// Release records the final ExecResult for this in-flight work and wakes
// any waiters. Must be called exactly once per successful Acquire.
func (t *IdempotencyTracker) Release(provider, key string, result ExecResult) { /* ... */ }

// Run starts the background GC goroutine that sweeps expired entries every
// SweepEvery. Stops when ctx is cancelled.
func (t *IdempotencyTracker) Run(ctx context.Context) { /* ... */ }
```

**Eviction policy.** Entries expire after `TTL` from acquisition. The GC sweeps expired entries every `SweepEvery`. If the map hits `MaxEntries` between sweeps, the oldest LRU entry is evicted — *not* the expiring one. This trades small probability of duplicate work (an evicted in-flight entry will let a duplicate through) for hard memory bounds.

**TTL choice.** The default 5 minutes is short enough to be cheap and long enough to absorb retry intervals from every provider on the roadmap (JSM retries are minutes-scale; GitHub Checks API retries are minutes-scale; Slack `chat.postMessage` is seconds-scale). Per-instance override is allowed (`Instance.IdempotencyTTL`).

### Routing

`POST /{provider_type}/{webhook_id}` is the only public webhook route. Examples:

```
POST /jsm/abc123def456     → JSM tenant A, K8s backend, namespace wiz-operator
POST /jsm/9zeq7lkm3wp4     → JSM tenant B, K8s backend, namespace wiz-operator-2
POST /github/7xkqp3l9zwer  → GitHub org X, AWS backend, region us-west-2
```

Path values are bound via Go 1.22+ `ServeMux` `r.PathValue("provider_type")` / `r.PathValue("webhook_id")`. The middleware chain in `httpx.Chain` is unchanged from DESIGN-0001 — recover, otel, request-id, rate-limit, slog, metrics — but the rate-limiter middleware's `providerFromPath` helper updates from `/webhook/<provider>` to `/<provider>/<id>` (per-provider rate-limit pools stay keyed on `provider_type`, not on `(provider_type, webhook_id)` — bursting one tenant shouldn't lock others out).

The `/webhook` URL prefix from the current shape is dropped. ADR-0001 (stdlib `net/http` routing) carries forward; multi-tenant routing builds on it.

## API / Interface Changes

### HTTP API

| Endpoint | Before | After |
|---|---|---|
| Public webhook | `POST /webhook/{provider}` | `POST /{provider_type}/{webhook_id}` |
| Admin readyz/livez/metrics/pprof | `GET /readyz` etc. | unchanged |

The admin listener, request-id propagation, OTel tracing, and Prometheus metrics endpoints are unchanged.

### CLI changes

Two new subcommands shipped with the binary:

- `webhookd config validate <path-or-dir>` — runs the full HCL parse + per-block `DecodeConfig` pipeline; exits non-zero with `hcl.Diagnostics` rendered to stderr on any error. Accepts either a single `.hcl` file or a directory. Used in CI / Helm chart pre-install hooks.
- `webhookd id generate` — emits a single fresh opaque webhook ID (12 base32 chars). Used by operators when defining a new instance.

`webhookd run` (the existing default behavior) reads `--config <path>` (single file) or `--config-dir <dir>` (directory of `*.hcl`); env-var fallback is `WEBHOOK_CONFIG_PATH` / `WEBHOOK_CONFIG_DIR`. Old env vars (`WEBHOOK_PROVIDERS`, `WEBHOOK_JSM_*`, `WEBHOOK_CR_*`, etc.) are removed; their replacement lives in HCL.

## Data Model

### Configuration HCL2 schema

Per ADR-0009, the wire format is HCL2. webhookd accepts either a single file (`--config /etc/webhookd/webhookd.hcl`) or a directory (`--config-dir /etc/webhookd/conf.d/`); HCL2's parser merges all `*.hcl` files in a directory at parse time. The Helm chart renders the config into a `ConfigMap`; secrets remain env-mapped via `existingSecret`.

```hcl
# webhookd.hcl — read at startup. Hot-reload not supported in v1.

defaults {
  idempotency_ttl = "5m"
  max_body_bytes  = 1048576
}

runtime {
  addr             = ":8080"
  admin_addr       = ":9090"
  shutdown_timeout = "30s"

  rate_limit {
    rps   = 50
    burst = 100
  }

  tracing {
    enabled  = true
    endpoint = "otel-collector.observability:4317"
  }

  pprof_enabled = false
  log_level     = "info"
  log_format    = "json"
}

instance "abc123def456" {
  provider "jsm" {
    trigger_status = "Approved"

    fields {
      provider_group_id = "customfield_10001"
      role              = "customfield_10002"
      project           = "customfield_10003"
    }

    signing {
      secret_env       = "WEBHOOK_JSM_TENANT_A_SECRET"   # always env, never inline
      signature_header = "X-Hub-Signature-256"
      timestamp_header = "X-Webhook-Timestamp"
      skew             = "5m"
    }
  }

  backend "k8s" {
    kubeconfig_env       = "KUBECONFIG_TENANT_A"  # empty = in-cluster
    namespace            = "wiz-operator"
    identity_provider_id = "tenant-a-idp"
    sync_timeout         = "20s"
  }

  # idempotency_ttl defaults to defaults.idempotency_ttl above
}

instance "7xkqp3l9zwer" {
  provider "github" {
    events = ["pull_request", "check_run"]
    signing {
      secret_env       = "WEBHOOK_GH_ORG_X_SECRET"
      signature_header = "X-Hub-Signature-256"
    }
  }

  backend "aws" {
    region    = "us-west-2"
    event_bus = "prod-events"
  }
}
```

The `instance "ID"` block uses HCL2's labeled-block form. The `provider "TYPE"` and `backend "TYPE"` nested blocks use the same pattern — TYPE is the registered `Provider.Type()` / `Backend.Type()`. Block-by-label is what makes per-integration typed decoding fall out of the parser for free.

### Validation: `gohcl` decoding (no separate JSON Schema)

Each Provider and Backend ships a Go struct with `hcl:""` tags. The dispatcher's loader calls `gohcl.DecodeBody(block.Body, evalCtx, &cfg)` per-block; HCL2 returns structured `hcl.Diagnostics` with file/line/column on any required-field, type, or unknown-key error. Validation and decoding are one step.

```go
// internal/integrations/jsm/config.go
type Config struct {
    TriggerStatus string        `hcl:"trigger_status"`
    Fields        FieldsConfig  `hcl:"fields,block"`
    Signing       SigningConfig `hcl:"signing,block"`
}

type FieldsConfig struct {
    ProviderGroupID string `hcl:"provider_group_id"`
    Role            string `hcl:"role"`
    Project         string `hcl:"project"`
}

type SigningConfig struct {
    SecretEnv       string        `hcl:"secret_env"`
    SignatureHeader string        `hcl:"signature_header,optional"`
    TimestampHeader string        `hcl:"timestamp_header,optional"`
    Skew            time.Duration `hcl:"skew,optional"`
}

// internal/integrations/jsm/provider.go
func (*Provider) DecodeConfig(body hcl.Body, evalCtx *hcl.EvalContext) (webhook.ProviderConfig, hcl.Diagnostics) {
    var cfg Config
    diags := gohcl.DecodeBody(body, evalCtx, &cfg)
    if diags.HasErrors() {
        return nil, diags
    }
    // additional cross-field validation here if needed
    return cfg, nil
}
```

The shared `evalCtx` exposes a small set of HCL2 functions the dispatcher whitelists — initially just `env("VAR")` for `${env("WEBHOOK_X_SECRET")}`-style references; expand only if a real use case shows up.

The `webhookd config validate` CLI subcommand runs the full parse + decode pipeline against a config path and exits non-zero on any diagnostic — Helm chart install hooks and CI pipelines fail fast without spinning up a webhookd server.

### In-memory `Config` struct

The on-disk structure decodes directly into typed Go structs via HCL2 tags — **no `RawConfigBlock` round-trip is needed**, because `gohcl.DecodeBody` accepts the raw `hcl.Body` and the decoder dispatches by block-label to per-integration `DecodeConfig` calls.

```go
// internal/config/config.go

type Config struct {
    Defaults  DefaultsBlock  `hcl:"defaults,block"`
    Runtime   RuntimeBlock   `hcl:"runtime,block"`
    Instances []InstanceBlock `hcl:"instance,block"`

    // Populated at startup (build-info, not from HCL).
    BuildInfo BuildInfo `hcl:"-"`
}

type DefaultsBlock struct {
    IdempotencyTTL time.Duration `hcl:"idempotency_ttl,optional"`
    MaxBodyBytes   int64         `hcl:"max_body_bytes,optional"`
}

type RuntimeBlock struct {
    Addr            string             `hcl:"addr,optional"`
    AdminAddr       string             `hcl:"admin_addr,optional"`
    ShutdownTimeout time.Duration      `hcl:"shutdown_timeout,optional"`
    RateLimit       *RateLimitBlock    `hcl:"rate_limit,block"`
    Tracing         *TracingBlock      `hcl:"tracing,block"`
    PProfEnabled    bool               `hcl:"pprof_enabled,optional"`
    LogLevel        string             `hcl:"log_level,optional"`
    LogFormat       string             `hcl:"log_format,optional"`
}

// InstanceBlock is the HCL form. Construction into a runtime *webhook.Instance
// happens in internal/config/instances.go via Provider.DecodeConfig +
// Backend.DecodeConfig — those operate on the per-block hcl.Body, *not* on
// re-marshaled bytes. No round-trip; full type safety end to end.
type InstanceBlock struct {
    ID             string             `hcl:"id,label"`
    IdempotencyTTL *time.Duration     `hcl:"idempotency_ttl,optional"`
    Provider       ProviderInstanceBlock `hcl:"provider,block"`
    Backend        BackendInstanceBlock  `hcl:"backend,block"`
}

type ProviderInstanceBlock struct {
    Type string   `hcl:"type,label"`   // "jsm", "github", etc.
    Body hcl.Body `hcl:",remain"`      // unparsed body, handed to Provider.DecodeConfig
}

type BackendInstanceBlock struct {
    Type string   `hcl:"type,label"`   // "k8s", "aws", "http", etc.
    Body hcl.Body `hcl:",remain"`      // unparsed body, handed to Backend.DecodeConfig
}
```

`config.Build(cfg *Config, reg *webhook.Registry) (*webhook.InstanceMap, hcl.Diagnostics)` is the constructor. For each `InstanceBlock`, it: looks up the Provider and Backend by `Type` in the registry; invokes their `DecodeConfig` against the `hcl.Body`; builds a `webhook.Instance`; inserts into the `InstanceMap`. All errors flow back as `hcl.Diagnostics` with full file/line context.

### K8s-side annotations carrying forward

The annotations webhookd stamps onto K8s resources are unchanged from IMPL-0002:

- `webhookd.io/managed-by: webhookd`
- `webhookd.io/source: <provider_type>` (was hardcoded "jsm"; now per-instance)
- `webhookd.io/trace-id: <32-hex>` (ADR-0007)
- `webhookd.io/request-id: <ulid>`
- `webhookd.io/jsm-issue-key: <key>` — *moves from a webhook-package constant to a JSM-Provider-specific stamp*. The K8s backend exposes a generic `Annotations` field on `ApplyCRRequest`; the JSM Provider populates `webhookd.io/jsm-issue-key` when it constructs the request.
- `webhookd.io/applied-at: <RFC3339>`

A new annotation, **`webhookd.io/callback-fired-at: <RFC3339>`**, is reserved for Phase 4 (per ADR-0008 §Decision and the second-level dedupe pattern). Phase 0–3 don't set or read it.

## Error Handling

### Error classification at each layer

| Layer | Error source | Maps to | HTTP |
|---|---|---|---|
| Routing | Unknown `(provider_type, id)` | `404 Not Found` | 404 |
| Body read | Body exceeds `MaxBodyBytes` | `http.MaxBytesError` → 413 | 413 |
| Signature | Provider's `VerifySignature` returns non-nil | `401 Unauthorized` | 401 |
| Idempotency key extraction | Provider's `IdempotencyKey` returns error | `ResultBadRequest` | 400 |
| Provider parse | `Handle` returns error wrapping `ErrBadRequest` | `ResultBadRequest` | 400 |
| Provider semantic | `Handle` returns error wrapping `ErrUnprocessable` | `ResultUnprocessable` | 422 |
| Provider unknown | `Handle` returns any other error | `ResultInternalError` | 500 |
| Wiring mismatch | `BackendRequest.BackendType() != Backend.Type()` | `ResultInternalError` | 500 |
| Backend execute | Backend returns `ResultKind` directly | per `HTTPStatus()` | varies |

The `webhook.ErrBadRequest` and `webhook.ErrUnprocessable` sentinels survive from `internal/webhook/action.go` essentially unchanged (move to `internal/webhook/errors.go`).

### HTTP status code mapping

`ResultKind.HTTPStatus()` is unchanged from IMPL-0002:

| ResultKind | HTTP |
|---|---|
| `ResultNoop` | 200 |
| `ResultReady` | 200 |
| `ResultTransientFailure` | 503 |
| `ResultBadRequest` | 400 |
| `ResultUnprocessable` | 422 |
| `ResultInternalError` | 500 |
| `ResultTimeout` | 504 |

### Logging and tracing

Every dispatcher path emits a structured slog line at INFO with: `provider_type`, `webhook_id`, `idempotency_key` (if any), `result_kind`, `duration_ms`, `trace_id`, `request_id`. WARN/ERROR escalation on signature failures, decode errors, and backend transient/internal failures.

Every backend call is wrapped in an OTel span (`backend.execute`) with attributes `backend.type`, `backend.outcome`, `webhook.provider_type`, `webhook.instance_id`. The K8s Backend's `k8s.apply` and `k8s.watch_cr` spans (from IMPL-0002 Phase 7) become children of `backend.execute`.

`webhook.instance_id` is a **per-span attribute**, not an OTel `Resource` attribute. Resource attributes are process-lifetime stable (they describe the binary, the host, the K8s pod) and apply uniformly to every span the process emits — instance_id is per-request and varies across the multi-tenant set, so it belongs on the span. Adding it to the resource would either lock the binary to a single instance (defeating multi-tenant) or require switching the resource per-request (semantically wrong and breaks OTel exporter caching).

Metrics survive from IMPL-0002 Phase 7 with one rename: `webhookd_jsm_*` metrics become `webhookd_provider_*{provider_type=...}` — drop the JSM-specific naming, label by provider type.

## Testing Strategy

### Unit

- `internal/webhook/registry_test.go` — registry duplicate-registration panic, isolated `NewRegistry` independence from globals.
- `internal/webhook/instance_test.go` — `InstanceMap.Lookup` happy path + miss.
- `internal/webhook/idempotency_test.go` — `Acquire` first-wins, second-blocks; `Release` wakes waiters; TTL eviction; LRU eviction at cap; concurrent acquire under race detector.
- `internal/webhook/result_test.go` — `HTTPStatus` exhaustive (one case per `ResultKind`).
- `internal/config/config_test.go` — HCL parse + per-block `gohcl.DecodeBody` happy path, malformed HCL syntax, unknown provider type, unknown backend type, missing required attributes (verified via `hcl.Diagnostics` shape), multi-file directory load.

### Per-integration

Each integration package owns its own tests, mirroring IMPL-0002's patterns:

- `internal/integrations/jsm/` — payload decode, extract sentinels, signature verification, fuzz target, full Provider compliance suite.
- `internal/integrations/k8s/` — apply path, watch path, classification, envtest scenarios.
- `internal/integrations/github/`, `aws/`, etc. — analogous.

A shared **compliance test suite** (`internal/webhook/providertest`, generalized from the existing `providertest/Mock`) exercises the Provider contract (signature failure → 401, decode failure → 400 wrapping `ErrBadRequest`, IdempotencyKey behavior, etc.) so each new Provider gets a known-good baseline.

### Dispatcher and registry

- `internal/webhook/dispatcher_test.go` — full request flow with mock Provider + mock Backend; routing failure (404), signature failure (401), idempotency duplicate (cached result), provider error classification, backend wiring mismatch (500), happy path.
- Configurations injected via `webhook.NewRegistry()` (no `init()` side effects in tests).

### End-to-end (envtest + kind)

- `cmd/webhookd/main_test.go` — boot `run()` against envtest with a generated HCL config + kubeconfig; post a signed JSM payload to `/jsm/<id>`; assert success.
- Smoke test in `make smoke-test` — kind cluster + a 2-instance HCL config (JSM + a stub HTTP backend), verifying multi-tenant routing works end-to-end.

### The Phase 3 abstraction-pressure test

When Phase 3's second integration lands, the success criterion is that **`internal/webhook/` is not modified** — every change is in `internal/integrations/<new>/` and one import line in `cmd/webhookd/main.go`. If that fails, the abstraction has a defect; revise interfaces before continuing.

## Migration / Rollout Plan

Rollout follows RFC-0001's five phases. The DESIGN-level migration concern is **the hard cutover from env vars to HCL at Phase 2** — nothing is live yet (per RFC-0001 Resolved Decision §5), so no env-compat shim, no `--legacy-env` flag.

**Pre-cutover (Phase 1 ships):**

- Existing single-instance JSM users continue to set env vars; behavior unchanged.
- New `Backend` interface and open `BackendRequest` are in place internally; old behavior preserved.

**Cutover (Phase 2 ships):**

- New webhookd binary refuses to start if `WEBHOOK_CONFIG_PATH` is unset *and* legacy env vars (`WEBHOOK_PROVIDERS`, `WEBHOOK_JSM_*`) are set — error message points at the migration runbook.
- Helm chart `values.yaml` schema breaks: `jsm.*`, `signing.*`, `crIdentityProviderID`, etc. → `instances` list rendered into HCL inside a `ConfigMap`. Major chart-version bump.
- Migration runbook (`docs/runbook/`) shows the env-var → HCL `instance "..." { ... }` mapping for the existing JSM workflow as a reference.
- `webhookd config validate` is the recommended pre-deployment check.

**Post-cutover:**

- Phase 3 adds the second integration without further breaking changes.
- Phase 4 introduces `AsyncBackend`; backends opt in.

## Resolved Decisions

The five questions raised during initial review are answered below. Reasoning preserved so future readers don't re-derive.

1. **Schema/decoding pipeline: HCL2 typed decoding via `gohcl`. No separate JSON Schema files.** During review the user pushed back on the YAML+JSON-Schema fragmentation cost, surfacing that HCL2's typed-decoding-plus-validation collapses both concerns into one Go-struct-with-tags artifact per integration. ADR-0009 was flipped from YAML to HCL2 in response. The Go struct + `hcl:""` tags *are* the schema; `gohcl.DecodeBody` validates and decodes in one step; `hcl.Diagnostics` carries file/line/column on errors. See ADR-0009 for the full rationale on the format choice.

2. **202-Accepted wire contract: in this DESIGN.** The dispatcher contract changes when async lands (a new `ResultKind`, new `ExecResult` fields, a new response body shape), and pinning the wire shape now prevents a second breaking change in Phase 4. See §AsyncBackend interface (Phase 4) → §202-Accepted response body. The full *lifecycle* (token persistence, callback poster HMAC, retry/timeout) still lives in DESIGN-0005 — only the wire contract is here.

3. **OTel instance_id: per-span attribute, not Resource attribute.** Resource attributes are process-lifetime stable; `webhook.instance_id` is per-request and varies across the multi-tenant set. Per-span is the only semantically correct option. See §Logging and tracing.

4. **`DecodeConfig` signature: takes `hcl.Body` + `*hcl.EvalContext`, returns `(ProviderConfig | BackendConfig, hcl.Diagnostics)`.** Q1's flip to HCL2 made this falls-out-of-the-design rather than a real choice. Each integration decodes against its typed struct via `gohcl.DecodeBody(body, evalCtx, &cfg)`; no raw bytes, no `map[string]any` round-trip, no re-marshal. Type-safe end to end. See §Provider interface and §Backend interface.

5. **Idempotency in-flight semantics: option (a) — return 200-noop "duplicate" immediately.** When a duplicate arrives while the first is still in-flight, the duplicate gets 200 with `status: "noop", reason: "duplicate"`. The tracker doesn't block the duplicate caller on a shared result channel. Reasoning: matches the failure model the user articulated ("JSM retried because we were slow; both retries do the same thing safely via SSA idempotency at the K8s layer"). Option (b) — block and replay the same result to both callers — is the platonic ideal, but it forces the tracker to outlive the request goroutine, and the small win (consistent result body across duplicates) doesn't justify the complexity. Revisit if a real failure mode shows up where 200-noop-duplicate misleads the caller.

## References

### Implements

- **RFC-0001** — *Generalize webhookd to Provider × Backend with Multi-Tenant Routing.* The proposal this DESIGN turns into concrete interfaces.

### Binding decisions

- **ADR-0006** — Synchronous response contract for webhook providers. The foundational contract this design preserves; `Backend.Execute` is the canonical synchronous shape.
- **ADR-0008** — Provider-callback pattern over durable queues for long-running work. Binds the `AsyncBackend` interface and the deferred Phase 4/5 plan.
- **ADR-0009** — HCL2 configuration format for multi-tenant instances. Binds the HCL2 block schema, the `gohcl.DecodeBody` typed-decoding path, and the in-memory `Config` struct shape (Go struct + `hcl:""` tags *are* the schema).
- **ADR-0010** — Static integration registration via build-time imports. Binds the `Registry` design (global + `NewRegistry` for test isolation) and the `init()` pattern.
- **ADR-0001** — stdlib `net/http` `ServeMux` for HTTP routing. The path-value-based multi-tenant routing builds on this.
- **ADR-0004** — controller-runtime typed client for Kubernetes access. Constrains the K8s Backend's client construction (Phase 0).
- **ADR-0005** — Server-Side Apply for custom resource reconciliation. Constrains the K8s Backend's apply path.
- **ADR-0007** — Trace-context propagation via CR annotation. Continues to apply to the K8s Backend.

### Background

- **INV-0001** — *Multi-Provider Multi-Backend Architecture Review.* The original investigation; resolved decisions are ratified into RFC-0001 + ADRs 0008/0009/0010.
- **INV-0002** — *Evaluate Argo Events as Alternative for JSM-to-CR Webhook Workflow.* The rejected-alternative investigation; conclusion ratified into ADR-0008 §Alternatives.

### Superseded / built on

- **DESIGN-0001 + IMPL-0001** — Stateless webhook receiver. Substrate carries forward unchanged.
- **DESIGN-0002 + IMPL-0002** — JSM webhook → SAMLGroupMapping CR provisioning. Status flips to *Implemented-but-superseded* when this DESIGN's Phase 2 lands. IMPL-0002 §Resolved Decisions is the source-of-truth for behavior that must survive the refactor.
- **DESIGN-0003 + IMPL-0003** — Helm chart and release pipeline. Phase 2 of this design requires chart updates to render HCL config (single `webhookd.hcl` or a directory of `.hcl` files) into a `ConfigMap`; the existing `existingSecret` pattern carries forward for `secret_env`-referenced credentials.
- **ADR-0003** — Environment-variable-only configuration. *Superseded by ADR-0009 when this DESIGN's Phase 2 lands.*
- **DESIGN-0005 (planned)** — Async Backend Callback Pattern. Will own the full `AsyncBackend` lifecycle (token persistence, callback poster contract, retry/timeout policy, `callback-fired-at` annotation handling). Written when Phase 4 is scheduled.
