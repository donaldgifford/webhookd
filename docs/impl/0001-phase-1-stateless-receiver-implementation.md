---
id: IMPL-0001
title: "Phase 1 Stateless Receiver Implementation"
status: Draft
author: Donald Gifford
created: 2026-04-25
---
<!-- markdownlint-disable-file MD025 MD041 -->

# IMPL 0001: Phase 1 Stateless Receiver Implementation

**Status:** Draft
**Author:** Donald Gifford
**Date:** 2026-04-25

<!--toc:start-->
- [Objective](#objective)
- [Scope](#scope)
  - [In Scope](#in-scope)
  - [Out of Scope](#out-of-scope)
- [Implementation Phases](#implementation-phases)
  - [Phase 0: Project Bootstrap](#phase-0-project-bootstrap)
    - [Tasks](#tasks)
    - [Success Criteria](#success-criteria)
  - [Phase 1: Configuration Package](#phase-1-configuration-package)
    - [Tasks](#tasks-1)
    - [Success Criteria](#success-criteria-1)
  - [Phase 2: Observability Substrate](#phase-2-observability-substrate)
    - [Tasks](#tasks-2)
    - [Success Criteria](#success-criteria-2)
  - [Phase 3: HTTP Middleware and Admin Mux](#phase-3-http-middleware-and-admin-mux)
    - [Tasks](#tasks-3)
    - [Success Criteria](#success-criteria-3)
  - [Phase 4: Webhook Handler & Signature Verification](#phase-4-webhook-handler--signature-verification)
    - [Tasks](#tasks-4)
    - [Success Criteria](#success-criteria-4)
  - [Phase 5: Application Wiring & Graceful Shutdown](#phase-5-application-wiring--graceful-shutdown)
    - [Tasks](#tasks-5)
    - [Success Criteria](#success-criteria-5)
  - [Phase 6: Operational Extras](#phase-6-operational-extras)
    - [Tasks](#tasks-6)
    - [Success Criteria](#success-criteria-6)
- [File Changes](#file-changes)
- [Testing Plan](#testing-plan)
- [Dependencies](#dependencies)
- [Resolved Decisions](#resolved-decisions)
- [References](#references)
<!--toc:end-->

## Objective

Land the Phase 1 receiver service described in DESIGN-0001: a single Go binary
that accepts signed webhook deliveries on a public listener, exposes
`/metrics`, `/healthz`, `/readyz` on a separate admin listener, emits
trace-correlated structured logs, and shuts down gracefully on signal.

The emphasis of Phase 1 is the **observability substrate** — every later
phase layers business logic onto the metrics, tracing, and logging plumbing
this implementation establishes. The webhook handler itself is intentionally
minimal: read body → verify HMAC + timestamp → parse generic envelope →
log → 202.

**Implements:** DESIGN-0001 (Stateless Webhook Receiver — Phase 1).

## Scope

### In Scope

- Project bootstrap: `go.mod`, `Dockerfile`, `docker-bake.hcl`, version
  ldflags wiring.
- Configuration package (`internal/config`) with all env vars from
  DESIGN-0001 §Configuration.
- Observability substrate: slog + trace correlation, OTel tracer provider
  with OTLP/HTTP exporter, Prometheus registry + instruments.
- HTTP middleware chain (recover, otelhttp, request_id, slog, metrics) and
  admin mux (`/healthz`, `/readyz`, `/metrics`).
- Webhook handler with HMAC-SHA256 signature verification, **timestamp-based
  replay protection**, generic envelope parse, domain-event log, 202 response.
- Application wiring in `cmd/webhookd/main.go` per the walk1.md startup
  phases (config → observability → handlers → servers → run loop).
- Graceful shutdown on SIGTERM/SIGINT.
- Tests at the levels DESIGN-0001 §Testing Strategy specifies: unit
  (table-driven), integration (full server spin-up), fuzz target on
  signature verification.
- Operational extras agreed during planning: `pprof` on the admin listener
  (for Pyroscope pull-based scraping), in-process per-provider rate
  limiting middleware.

### Out of Scope

- Provider registry / dispatcher / `Provider` interface — Phase 2.
- Kubernetes CR apply path, controller-runtime client — Phase 2.
- Helm chart and K8s manifests — separate impl doc / branch.
- SLOs, alert rules, Grafana dashboards — separate ops doc.
- Multi-provider signing secret layout — deferred (DESIGN-0001 Open
  Questions).

## Implementation Phases

Each phase builds on the previous. A phase is complete when all its tasks
are checked off and its success criteria are met. Phases are sized to be
landable as individual commits or small follow-up PRs, not as one
mega-merge.

---

### Phase 0: Project Bootstrap

Establishes the Go module, container build, and CI's docker-bake target so
later phases can run `make ci` to green. No business logic.

#### Tasks

- [ ] Run `go mod init github.com/donaldgifford/webhookd` (Go version per
      `mise.toml`: 1.26.1).
- [ ] Pin core third-party dependencies in `go.mod`:
  - [ ] `github.com/prometheus/client_golang`
  - [ ] `go.opentelemetry.io/otel`
  - [ ] `go.opentelemetry.io/otel/sdk`
  - [ ] `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp`
  - [ ] `go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp`
- [ ] Add a minimal `cmd/webhookd/main.go` placeholder that builds (just
      `package main; func main() {}` — real wiring lands in Phase 5) so
      `make build` succeeds.
- [ ] Wire `-ldflags "-X main.version -X main.commit"` (already in the
      Makefile). Add matching package-level `var version, commit string`
      in `cmd/webhookd/main.go`.
- [ ] Write a `Dockerfile`: multi-stage, distroless static base; non-root
      user; build with the same ldflags.
- [ ] Write a `docker-bake.hcl` matching the targets the existing
      `.github/workflows/ci.yml` `docker-build` job invokes (`ci` target
      at minimum).
- [ ] Verify `make ci` is green locally (lint + test + build +
      license-check). Tests will be empty at this point — that's expected.

#### Success Criteria

- `make build` produces `build/bin/webhookd` and the binary prints version
  info if invoked with `--version` (or simply runs without crashing).
- `make ci` runs end-to-end with no failures (no real tests yet; lint
  must pass on the empty packages).
- `docker buildx bake ci` builds the image locally.
- CI on a draft PR turns green for the lint, test, build, and
  docker-build jobs.

---

### Phase 1: Configuration Package

`internal/config` — env-var parsing, validation, defaults. Walk1.md §3
gives the canonical shape; this phase implements it.

#### Tasks

- [ ] Create `internal/config/config.go` with the `Config` struct holding
      all 18 env vars from DESIGN-0001 §Configuration plus a nested
      `BuildInfo{Version, Commit, GoVersion}`.
- [ ] Implement small typed helpers: `envString`, `envDuration`,
      `envInt64`, `envBool`. Each ~10 lines, no third-party deps.
- [ ] Implement `Load() (*Config, error)`:
  - [ ] Reads every variable, applies defaults.
  - [ ] Returns an error for missing required values
        (`WEBHOOK_SIGNING_SECRET`).
  - [ ] Validates `WEBHOOK_TRACING_SAMPLE_RATIO` is in `[0.0, 1.0]`.
  - [ ] Validates `WEBHOOK_LOG_LEVEL` parses to a `slog.Level`.
  - [ ] Validates `WEBHOOK_LOG_FORMAT` is `json` or `text`.
- [ ] Add the **Phase 1 replay-protection vars** that DESIGN-0001 does
      not yet enumerate:
  - [ ] `WEBHOOK_SIGNATURE_HEADER` (default `X-Webhook-Signature`).
  - [ ] `WEBHOOK_TIMESTAMP_HEADER` (default `X-Webhook-Timestamp`).
  - [ ] `WEBHOOK_TIMESTAMP_SKEW` (default `5m`).
- [ ] Add the Phase 1 rate-limit vars:
  - [ ] `WEBHOOK_RATE_LIMIT_RPS` (default `100`).
  - [ ] `WEBHOOK_RATE_LIMIT_BURST` (default `200`).
- [ ] Write table-driven tests in `internal/config/config_test.go`
      covering: defaults, every override, type errors, missing-required,
      boundary conditions (`SAMPLE_RATIO=-0.1` and `1.1`).
- [ ] Use `t.Setenv` (not `os.Setenv`) so tests parallelize cleanly.

#### Success Criteria

- `go test ./internal/config/...` passes with `-race`.
- Coverage on `internal/config` ≥90% (single package, easy bar).
- Loading with no env vars set fails fast with a clear message naming
  `WEBHOOK_SIGNING_SECRET`.
- All env vars in DESIGN-0001 §Configuration plus the replay-protection
  and rate-limit additions appear in the struct and have matching tests.

---

### Phase 2: Observability Substrate

`internal/observability` — the three subsystems that every later phase
depends on (logging, tracing, metrics). Walk1.md §4 is the canonical
reference.

#### Tasks

**Logging — `internal/observability/logging.go`:**

- [ ] Implement `NewLogger(level slog.Level, format string) *slog.Logger`
      that returns either a JSON or text handler wrapped in a custom
      `traceHandler`.
- [ ] Implement `traceHandler` that, in `Handle(ctx, record)`, looks up
      the active span via `trace.SpanFromContext(ctx)` and adds
      `trace_id` and `span_id` attributes when the span context is
      valid.
- [ ] Tests: emit a log inside an active span, assert the rendered JSON
      carries both attrs; emit without a span, assert no attrs added
      and no error.

**Tracing — `internal/observability/tracing.go`:**

- [ ] Implement `NewTracerProvider(ctx, cfg) (*sdktrace.TracerProvider, error)`:
  - [ ] If `cfg.TracingEnabled == false`, return a no-op-ish provider
        with `sdktrace.NewTracerProvider()` (no exporter).
  - [ ] Otherwise, build OTLP/HTTP exporter via `otlptracehttp.New(ctx)`
        — let it read `OTEL_EXPORTER_OTLP_*` env vars natively.
  - [ ] Build resource via `resource.New(ctx, resource.WithFromEnv(),
        resource.WithAttributes(semconv.ServiceName, semconv.ServiceVersion))`.
  - [ ] Wire batch span processor.
  - [ ] Set sampler via `samplerFor(ratio)` helper:
        `>=1.0 → ParentBased(AlwaysSample())`,
        `<=0.0 → ParentBased(NeverSample())`,
        else `ParentBased(TraceIDRatioBased(ratio))`.
- [ ] Tests for `samplerFor` boundary behavior (1.0, 0.0, 0.5, 1.5,
      -0.1).

**Metrics — `internal/observability/metrics.go`:**

- [ ] Define `Metrics` struct holding all instruments listed in
      DESIGN-0001 §Metrics:
  - [ ] HTTP layer: `HTTPRequests`, `HTTPDuration`, `HTTPRequestSize`,
        `HTTPResponseSize`, `HTTPInflight`, `HTTPPanics`.
  - [ ] Webhook domain: `WebhookEvents`, `WebhookSigResults`,
        `WebhookProcessing`.
  - [ ] Rate-limit field added in Phase 6 — backfill the struct then.
- [ ] Implement `NewMetrics(build BuildInfo) (*prometheus.Registry, *Metrics)`
      that:
  - [ ] Creates a fresh `prometheus.NewRegistry()` (not `DefaultRegisterer`).
  - [ ] Registers all instruments with the canonical names + labels +
        bucket sets from DESIGN-0001 §Metrics.
  - [ ] Registers `collectors.NewGoCollector()` and
        `collectors.NewProcessCollector()`.
  - [ ] Registers a `webhookd_build_info{version, commit, go_version} = 1`
        constant collector.
- [ ] Implement `BuildInfo` constant collector (small wrapper around
      `prometheus.NewGaugeFunc` or `MustNewConstMetric` with
      const-labels populated from `BuildInfo`).
- [ ] Tests: spin up `NewMetrics`, scrape via `promhttp.HandlerFor(reg, ...)`,
      assert all metric names appear in the exposition output and
      `webhookd_build_info` is 1 with the expected label values.

#### Success Criteria

- `go test ./internal/observability/...` passes with `-race`.
- A log line emitted inside a span includes the span's trace_id/span_id
  in the JSON output (verified by test).
- A scrape of the metrics handler returns Prometheus exposition format
  containing every instrument name from DESIGN-0001 §Metrics plus the
  `go_*`, `process_*`, and `webhookd_build_info` collectors.
- `samplerFor` returns the right sampler type for each ratio bucket
  (verified by test asserting on `Sampler.Description()`).

---

### Phase 3: HTTP Middleware and Admin Mux

`internal/httpx` — the framework around the handler. Middleware order is
load-bearing; see Walk1.md §5.

#### Tasks

**Middleware — `internal/httpx/middleware.go`:**

- [ ] Implement `Chain(h http.Handler, mws ...func(http.Handler) http.Handler) http.Handler`
      that wraps in outermost-first order.
- [ ] Implement `Recover(metrics *observability.Metrics)`:
  - [ ] `defer recover()`; on panic, log at `error` with stack via
        `debug.Stack()`, increment `HTTPPanics`, write 500.
- [ ] Implement `OTel(serviceName string)` — a thin wrapper around
      `otelhttp.NewHandler(next, "")` so `main.go` doesn't import
      `otelhttp` directly.
- [ ] Implement `RequestID()`:
  - [ ] Read `X-Request-ID` header; if empty, generate a UUIDv7 via
        `github.com/google/uuid` (`uuid.NewV7().String()`).
  - [ ] Store in context via a typed key (`type ctxKey struct{}`).
  - [ ] Echo back as `X-Request-ID` response header.
- [ ] Implement `SLog()`:
  - [ ] Wrap response writer to capture status code and bytes written
        (reuse a `statusRecorder` helper).
  - [ ] On return, emit one log line at `info` with `method`, `route`,
        `status`, `bytes_in`, `bytes_out`, `duration_ms`, `request_id`,
        plus auto-injected `trace_id`/`span_id` via the traceHandler.
- [ ] Implement `Metrics(metrics *observability.Metrics)`:
  - [ ] `Inc/Dec` on `HTTPInflight`, observe `HTTPDuration`,
        `HTTPRequestSize`, `HTTPResponseSize`, increment `HTTPRequests`.
  - [ ] Use `r.Pattern` (Go 1.22+) for the `route` label; substitute
        `"__unmatched__"` if empty (404 path).
- [ ] Tests, each middleware in isolation:
  - [ ] `Recover` catches panic → 500 + counter increment.
  - [ ] `RequestID` populates context and echoes header.
  - [ ] `SLog` emits exactly one line with expected attrs.
  - [ ] `Metrics` records on 2xx, 4xx, 5xx; `HTTPInflight` returns to
        zero after the handler.

**Admin mux — `internal/httpx/admin.go`:**

- [ ] Implement `NewAdminMux(reg *prometheus.Registry, ready *atomic.Bool) http.Handler`:
  - [ ] `GET /healthz` → always 200.
  - [ ] `GET /readyz` → 200 if `ready.Load()`, 503 otherwise.
  - [ ] `GET /metrics` → `promhttp.HandlerFor(reg, ...)`.
  - [ ] Wrap the mux in only `Recover` and `Metrics` middleware (no
        otelhttp, no slog at info — would be too noisy for probes).

**Server constructor — `internal/httpx/server.go`:**

- [ ] Implement `NewServer(addr string, h http.Handler, cfg *config.Config) *http.Server`
      with `ReadTimeout`, `ReadHeaderTimeout`, `WriteTimeout`,
      `IdleTimeout` from config.
- [ ] Set `BaseContext` so request contexts inherit a parent the
      shutdown sequence can cancel.

#### Success Criteria

- `go test ./internal/httpx/...` passes with `-race`.
- Admin mux: hitting `/readyz` returns 503 before `ready.Store(true)`,
  200 after.
- Middleware composition order is locked in tests: a recover-only stack
  catches a downstream panic; a metrics-only stack records correct
  status; the full chain produces all expected side-effects exactly
  once.

---

### Phase 4: Webhook Handler & Signature Verification

`internal/webhook` — the actual webhook intake. This is where Phase 1's
**replay protection** lives (added per the conversation that produced
this impl doc; not in DESIGN-0001 explicitly).

#### Tasks

**Signature module — `internal/webhook/signature.go`:**

- [ ] Implement `VerifyHMAC(secret []byte, canonical []byte, received string) error`:
  - [ ] Parse `received` of form `sha256=<hex>` (return `ErrMalformed`
        for any other shape).
  - [ ] Compute `hmac.New(sha256.New, secret)` over `canonical`.
  - [ ] `hmac.Equal` for timing-safe compare.
  - [ ] Return `nil` on success, typed error on failure
        (`ErrInvalidSignature`).
- [ ] Implement `VerifyTimestamp(headerVal string, now time.Time, skew time.Duration) error`:
  - [ ] Parse Unix seconds; reject anything older or newer than `skew`.
  - [ ] Return typed errors (`ErrTimestampMissing`,
        `ErrTimestampMalformed`, `ErrTimestampSkewed`).
- [ ] Implement a per-handler `Verify(secret, sigHeader, tsHeader, body, now, skew) error`
      that composes the two: build canonical = `"v0:" + tsHeader + ":" + body`
      (Slack-style with a version prefix so the scheme can be revved later
      without breaking signers), then verify both. Canonical format
      documented in code comments so future provider implementations
      follow the same shape.
- [ ] Tests with table-driven vectors:
  - [ ] Known-good signature → nil.
  - [ ] Wrong secret → `ErrInvalidSignature`.
  - [ ] Tampered body → `ErrInvalidSignature`.
  - [ ] Malformed header (`md5=...`, `sha256=zzz`, empty) →
        `ErrMalformed`.
  - [ ] Timestamp skew at boundary (`±skew`) and beyond.
  - [ ] Missing timestamp header → `ErrTimestampMissing`.
- [ ] Add `FuzzSignatureVerify` fuzz target seeded with valid and
      malformed header strings.

**Handler — `internal/webhook/handler.go`:**

- [ ] Implement `NewHandler(cfg HandlerConfig, metrics *observability.Metrics) http.Handler`
      where `HandlerConfig` carries the signing secret, max body bytes,
      header names, and skew.
- [ ] Handler flow per DESIGN-0001 §Webhook Handler Flow:
  - [ ] `provider := r.PathValue("provider")`.
  - [ ] `body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, cfg.MaxBodyBytes))`
        — on error, 413 if too large, else 400.
  - [ ] Start `webhook.verify_signature` span.
  - [ ] Call `signature.Verify(...)`; record
        `WebhookSigResults{provider, result}` (`valid|invalid|missing`).
  - [ ] On invalid → 401, increment
        `WebhookEvents{outcome="invalid_signature"}`, return.
  - [ ] Start `webhook.parse` span; `json.Unmarshal` into
        `Envelope{EventType string, Data json.RawMessage}`.
  - [ ] On malformed → 400, increment
        `WebhookEvents{outcome="malformed"}`, return.
  - [ ] Emit domain event log via `slog.InfoContext` with provider,
        event_type, payload size, request_id, trace_id (auto).
  - [ ] 202 Accepted; increment `WebhookEvents{outcome="accepted"}`,
        observe `WebhookProcessing`.
- [ ] Tests using `httptest.NewRecorder` and `testutil.ToFloat64`:
  - [ ] Happy path: valid signature + body → 202, counters incremented.
  - [ ] Invalid signature → 401, `signature_validation{result=invalid}`
        and `events{outcome=invalid_signature}` both incremented.
  - [ ] Body too large → 413, no envelope counter incremented.
  - [ ] Malformed JSON → 400, `events{outcome=malformed}` incremented.
  - [ ] Missing timestamp → 401, `signature_validation{result=missing}`
        incremented.

#### Success Criteria

- `go test ./internal/webhook/...` passes with `-race` and `-fuzz=Fuzz`
  for at least 60 seconds without findings.
- Coverage on `internal/webhook` ≥85%.
- Signature verification rejects every documented bad-input class with
  the typed error the test suite expects.
- Handler emits exactly one domain-event log line per accepted
  delivery, and the line carries `trace_id` and `span_id`.

---

### Phase 5: Application Wiring & Graceful Shutdown

`cmd/webhookd/main.go` — the only place all packages meet. Walk1.md §2
gives the canonical 5-phase startup; this implementation matches it
verbatim.

#### Tasks

- [ ] Replace the Phase 0 placeholder `main.go` with the full
      `main` + `run(ctx)` structure from Walk1.md §2.
- [ ] Phase A wiring: `config.Load()`, fail-fast on error to stderr,
      exit 1.
- [ ] Phase B wiring:
  - [ ] `observability.NewLogger(...)`, `slog.SetDefault(...)`.
  - [ ] `observability.NewTracerProvider(ctx, cfg)`,
        `otel.SetTracerProvider`, `otel.SetTextMapPropagator(
        propagation.NewCompositeTextMapPropagator(TraceContext{}, Baggage{}))`.
  - [ ] Defer `tp.Shutdown(...)` with a fresh timeout context bounded
        by `cfg.ShutdownTimeout`.
  - [ ] `observability.NewMetrics(buildInfo)`.
- [ ] Phase C wiring:
  - [ ] Public mux: `mux.Handle("POST /webhook/{provider}", webhook.NewHandler(...))`.
  - [ ] Compose middleware chain via `httpx.Chain`.
  - [ ] `readiness := &atomic.Bool{}`.
  - [ ] Build admin mux via `httpx.NewAdminMux(reg, readiness)`.
- [ ] Phase D wiring: `httpx.NewServer` for both addresses.
- [ ] Phase E wiring:
  - [ ] Goroutine per server, results into a buffered `errCh`.
  - [ ] `readiness.Store(true)` after both servers are dispatched.
  - [ ] Log "listening" with both addresses.
  - [ ] `waitForShutdown` helper: select on `errCh` or
        `signal.NotifyContext(SIGTERM, SIGINT)`.
- [ ] `waitForShutdown` shutdown sequence per DESIGN-0001 §Graceful
      Shutdown:
  - [ ] `readiness.Store(false)`.
  - [ ] `srv.Shutdown(ctx)` for both, with `cfg.ShutdownTimeout` budget.
  - [ ] Tracer provider shutdown via the deferred call.
  - [ ] Return error if anything exceeded budget so `main` exits 1.
- [ ] Set the build-time injected `version` and `commit` package vars
      and propagate them into `BuildInfo` for the metrics collector.
- [ ] Integration test in `cmd/webhookd/main_test.go` (or
      `internal/integration_test.go`):
  - [ ] `TestMain` wraps `goleak.VerifyTestMain(m)` from
        `go.uber.org/goleak` to catch goroutine leaks (BatchSpanProcessor,
        listeners, anything else).
  - [ ] Bind both servers to `127.0.0.1:0`.
  - [ ] Use `tracetest.NewInMemoryExporter` instead of OTLP/HTTP.
  - [ ] POST a signed payload to the public listener.
  - [ ] Scrape the admin listener; assert
        `webhookd_webhook_events_total{outcome="accepted"} == 1`.
  - [ ] Assert the in-memory exporter received at least one span.
  - [ ] Trigger graceful shutdown; assert clean exit.

#### Success Criteria

- `make run-local` starts both listeners and `curl -s :9090/healthz`
  returns 200, `curl -s :9090/readyz` returns 200, `curl -s :9090/metrics`
  returns Prometheus exposition with `webhookd_build_info` present.
- Sending SIGTERM to a running process produces a clean exit 0 within
  `WEBHOOK_SHUTDOWN_TIMEOUT`, with no goroutine-leak warnings if
  `goleak` is wired into the integration test.
- The integration test passes in `make ci` and is marked `-race`-clean.

---

### Phase 6: Operational Extras

The "must resolve before prod" items from the planning conversation:
pprof for Pyroscope, rate limiting, and license-header consistency.
These are additive and can land as their own commits / PRs.

#### Tasks

**pprof on admin listener (pull-based Pyroscope posture, no SDK import):**

- [ ] In `internal/httpx/admin.go`, register `net/http/pprof` handlers
      under `/debug/pprof/` on the admin mux only.
- [ ] Add `WEBHOOK_PPROF_ENABLED` env var (default `true`); skip
      registration when false (kill switch for paranoid environments).
- [ ] Document in DESIGN-0001 References that Pyroscope scrapes
      `/debug/pprof/profile` from the admin port — same pull model as
      Prometheus on `/metrics`. No `github.com/grafana/pyroscope-go` SDK
      import. Per-provider profile labels via `runtime/pprof.Do` are a
      Phase 2+ concern, deferred until multiple providers exist.

**Rate limiting middleware:**

- [ ] Add `internal/httpx/ratelimit.go` with an in-process,
      per-provider, global (per replica) token-bucket middleware.
- [ ] Use `golang.org/x/time/rate.Limiter`; one limiter per provider,
      created lazily, keyed on the path-pattern value. Defaults from
      config: `100 rps` rate, `200` burst — generous day-one settings,
      tighten via observability after first week of real traffic.
- [ ] On exceeded → 429 with `Retry-After` header, increment a new
      `webhookd_http_rate_limited_total{provider}` counter. Backfill
      this field on the `Metrics` struct if not already added in Phase 2.
- [ ] Insert in the chain after `RequestID` and before `SLog` so
      rate-limited requests get a request_id and a log line.
- [ ] Tests: drive a bucket past its rate, assert 429 + counter
      increment + `Retry-After` header presence.

**License header consistency:**

- [ ] Add a top-level `LICENSE` file with the standard Apache-2.0 text.
- [ ] Add `licenses-header.txt` with the SPDX-style two-line header:

      ```
      // SPDX-License-Identifier: Apache-2.0
      // Copyright 2026 webhookd contributors
      ```

- [ ] Configure `goheader` in `.golangci.yml` to point at the template
      and the `year`/`copyright-holder` values.
- [ ] Apply the header to all `*.go` files via `make fmt` or a
      `goheader -fix` pass.

#### Success Criteria

- `curl -s :9090/debug/pprof/heap > /dev/null` succeeds and returns a
  non-empty body.
- A load test driving the public listener past
  `WEBHOOK_RATE_LIMIT_RPS` produces `429` responses and the
  `webhookd_http_rate_limited_total` counter increments.
- `golangci-lint run --enable goheader` passes; every Go file has the
  agreed header.
- `make ci` is fully green, ready for cutover deploy.

---

## File Changes

| File | Action | Description |
|------|--------|-------------|
| `go.mod`, `go.sum` | Create | Module init + pinned deps. |
| `Dockerfile` | Create | Multi-stage build, distroless static base. |
| `docker-bake.hcl` | Create | Bake targets matching `ci.yml`. |
| `cmd/webhookd/main.go` | Modify | Full wiring per Walk1.md §2 (Phase 5). |
| `internal/config/config.go` | Create | Env parsing, `Config` struct. |
| `internal/config/config_test.go` | Create | Table-driven tests. |
| `internal/observability/logging.go` | Create | slog + traceHandler. |
| `internal/observability/tracing.go` | Create | OTel tracer provider. |
| `internal/observability/metrics.go` | Create | Prometheus registry + `Metrics`. |
| `internal/observability/*_test.go` | Create | Per-file tests. |
| `internal/httpx/middleware.go` | Create | Recover, OTel, RequestID, SLog, Metrics, Chain. |
| `internal/httpx/admin.go` | Create | Admin mux + pprof. |
| `internal/httpx/server.go` | Create | `*http.Server` constructor. |
| `internal/httpx/ratelimit.go` | Create | Per-provider token-bucket middleware. |
| `internal/httpx/*_test.go` | Create | Middleware unit tests. |
| `internal/webhook/signature.go` | Create | HMAC + timestamp verification. |
| `internal/webhook/handler.go` | Create | Webhook intake handler. |
| `internal/webhook/*_test.go` | Create | Unit + fuzz tests. |
| `licenses-header.txt` | Create | Source for `goheader` linter. |
| `.golangci.yml` | Modify | Point `goheader` at the new file. |

## Testing Plan

- **Unit tests** (per Phase 1–4 task lists) — table-driven, stdlib
  `testing`, `-race` enabled. Coverage targets: `config` ≥90%, `webhook`
  ≥85%, every other package ≥80%.
- **Fuzz target** — `FuzzSignatureVerify` in Phase 4; required to run
  60+ seconds clean before merge.
- **Integration test** — Phase 5 end-to-end test in `cmd/webhookd`,
  using `tracetest.NewInMemoryExporter` for traces and a fresh
  Prometheus registry. Must run inside `make test` (no separate target,
  no envtest — Phase 1 doesn't touch K8s).
- **Load test** — out-of-tree `vegeta` profile in `test/load/` (Phase 6
  optional; not run in CI).

## Dependencies

Direct module imports introduced by this implementation:

- `github.com/prometheus/client_golang` — metrics SDK + collectors +
  testutil.
- `go.opentelemetry.io/otel`, `.../sdk`, `.../sdk/trace/tracetest` —
  tracer provider + in-memory exporter for tests.
- `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp` —
  OTLP/HTTP exporter.
- `go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp` —
  automatic HTTP server instrumentation.
- `github.com/google/uuid` — UUIDv7 request IDs (Phase 3). Forward-
  compatible with the upcoming stdlib `uuid` in Go 1.27.
- `golang.org/x/time/rate` — token-bucket rate limiter (Phase 6).
- `go.uber.org/goleak` — goroutine-leak detection in the Phase 5
  integration test only (test-only dep).

No client-go, no controller-runtime, no JSM SDK — all of those land in
Phase 2. No Pyroscope SDK — profiling is pull-based.

## Resolved Decisions

These are the decisions made during impl-doc review. They started as
open questions; the answers are now baked into the phase tasks above.
Kept here so future readers can see the reasoning rather than just the
outcome.

1. **Phase 1 provider names — accept any.** The handler accepts any
   `{provider}` path value; metric cardinality is bounded because only
   one signing secret produces valid signatures. Phase 2's
   `WEBHOOK_PROVIDERS` allow-list supersedes this. Likely real
   providers: github, discord, jsm.
2. **Replay-protection header — `X-Webhook-Timestamp`, 5-minute skew.**
   Both overridable via `WEBHOOK_TIMESTAMP_HEADER` and
   `WEBHOOK_TIMESTAMP_SKEW`. Real providers each use their own header
   shape; Phase 1 picks its own convention because no two upstream
   conventions match. Phase 2 providers read each provider's actual
   header in their own `VerifySignature`.
3. **Canonical signing string — `v0:<timestamp>:<body>` (Slack-style).**
   The `v0:` prefix means we can rev the scheme later (`v1:`...)
   without breaking signers. Stripe is low-likelihood for this service,
   so the version-prefix tradeoff is worth the byte.
4. **Request ID generator — UUIDv7 via `github.com/google/uuid`.**
   Time-sortable, plays well with OTel attribute strings, forward-
   compatible with stdlib `uuid` in Go 1.27 (mechanical migration when
   it lands).
5. **Rate-limit defaults — 100 rps rate, 200 burst, per-provider,
   per-replica (in-process), global.** Generous day-one headroom over
   expected 10–20 rps; both env-overridable; tighten via observability
   after first week of real traffic.
6. **License — Apache-2.0 with SPDX-style header.** Two-line per-file
   header (`SPDX-License-Identifier: Apache-2.0` + `Copyright 2026
   webhookd contributors`). Standard `LICENSE` file at repo root.
   Configured via `goheader` linter in Phase 6.
7. **Dockerfile base — `gcr.io/distroless/static-debian12:nonroot`.**
   Multi-stage build, `CGO_ENABLED=0`. Debugging via `kubectl debug
   --image=...` ephemeral containers, not via shell in the runtime
   image.
8. **Body reading — read-then-verify.** Body buffered into a `[]byte`
   bounded by `WEBHOOK_MAX_BODY_BYTES`, HMAC computed over the buffer,
   JSON parsed from the same buffer. Memory cost trivial at projected
   volume; lets us include `payload_bytes` as a cheap span attribute.
9. **`goleak` in the integration test only.** `TestMain` wraps
   `goleak.VerifyTestMain(m)` in `cmd/webhookd/main_test.go`. Catches
   leaks in BatchSpanProcessor, listeners, etc. Not added to unit-test
   packages where it's overkill.
10. **No `/readyz` gate on OTel exporter.** Tracing is degraded-not-
    fatal: a flaky collector should fire alerts, not take webhookd out
    of rotation. Replicas report ready as soon as listeners bind.
11. **Pyroscope is pull-based — no SDK import.** Pyroscope scrapes
    `/debug/pprof/profile` from the admin port the same way Prometheus
    scrapes `/metrics`. When multiple providers exist (Phase 2+),
    per-provider profile labels can be added by wrapping per-provider
    code in `runtime/pprof.Do(ctx, pprof.Labels("provider", name))` —
    no scrape-posture change needed.
12. **Helm chart split into its own impl doc / branch.** Phase 1
    stops at "container runs locally." Chart, K8s manifests,
    ServiceMonitor, Pyroscope scrape config, and deployment-environment
    extras (Gateway API for EKS, Tailscale sidecars, etc.) live in a
    follow-up impl doc.

## References

- DESIGN-0001 — Stateless Webhook Receiver Phase 1 (the source of truth
  for what to build).
- `archive/walk1.md` (gitignored) — line-by-line implementation
  walkthrough; canonical reference for package-level shape, middleware
  ordering, and startup phases. To be migrated to `docs/impl/` in a
  follow-up.
- ADR-0001 — Use stdlib net/http ServeMux for HTTP routing.
- ADR-0002 — Prometheus for metrics, OpenTelemetry for traces.
- ADR-0003 — Environment-variable-only configuration.
- OpenTelemetry Go SDK: <https://github.com/open-telemetry/opentelemetry-go>
- `prometheus/client_golang` testutil:
  <https://pkg.go.dev/github.com/prometheus/client_golang/prometheus/testutil>
- `golang.org/x/time/rate`: <https://pkg.go.dev/golang.org/x/time/rate>
