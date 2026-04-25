---
id: ADR-0002
title: "Prometheus for metrics, OpenTelemetry for traces"
status: Accepted
author: Donald Gifford
created: 2026-04-24
---

<!-- markdownlint-disable-file MD025 MD041 -->

# 0002. Prometheus for metrics, OpenTelemetry for traces

<!--toc:start-->
- [Status](#status)
- [Context](#context)
- [Decision](#decision)
- [Consequences](#consequences)
  - [Positive](#positive)
  - [Negative](#negative)
  - [Neutral](#neutral)
- [Alternatives Considered](#alternatives-considered)
- [References](#references)
<!--toc:end-->

## Status

Accepted

## Context

OpenTelemetry's stated goal is to be the unified SDK for metrics, traces, and
logs. On paper, using the OTel SDK for both signals would give us one dependency
tree, one resource model, one propagator set, and a consistent attribute
vocabulary across signals.

In practice, for webhookd today:

- Our backend for metrics is Prometheus (pull-based ServiceMonitor scrape). The
  `prometheus/client_golang` package is the native client — no translation
  layer, no collector in the hot path, existing Grafana dashboards assume
  Prometheus metric naming.
- Our backend for traces is an OTel Collector feeding Tempo. OTLP/HTTP is the
  native wire format; `otelhttp` gives us automatic HTTP server instrumentation
  with zero bespoke code.
- OTel's metrics SDK (Go) is stable but younger than the Prometheus client.
  Using it means a Prometheus exporter in the middle (more moving parts, more
  places for a cardinality bug to hide).
- The operational cost of "metrics look slightly different from every other
  service" at scrape/dashboard time is not trivial.

Unifying onto OTel metrics is a migration we can do later once the OTel SDK has
clearly surpassed the Prometheus client in ergonomics and stability for our
patterns. Doing it now buys us uniformity we do not need and costs us
battle-tested tooling we already have.

## Decision

Instrument webhookd with two separate SDKs, one per signal:

- **Metrics:** `github.com/prometheus/client_golang` on a dedicated
  `*prometheus.Registry`. Exposed on the admin listener at `:9090/metrics`.
- **Traces:** OpenTelemetry Go SDK with OTLP/HTTP exporter, `otelhttp` for
  automatic HTTP server spans, and manual `tracer.Start` calls for business
  operations.

Logs (`log/slog`) sit alongside both: they pick up `trace_id` / `span_id` from
the OTel span in the request context via a thin custom `slog.Handler`, so
log–trace correlation works without coupling the metrics and tracing SDKs.

This decision is explicitly Phase 1's posture. Revisiting it is a Phase 3+
conversation and should be driven by a concrete need (e.g. exemplar linking that
only OTel metrics support, or collector-side metric processing we want to do).

## Consequences

### Positive

- Native Prometheus semantics — histogram bucket configuration, label
  cardinality discipline, `testutil.ToFloat64` in tests all work the way our
  existing Grafana dashboards expect.
- Native OTel tracing — `otelhttp` provides automatic HTTP semantic conventions
  without us re-implementing them.
- `otlptracehttp.New` reads the standard `OTEL_EXPORTER_OTLP_*` env vars so we
  don't duplicate config surface for tracing.
- Neither SDK has to carry the other's weight — upgrade schedules, vulnerability
  surfaces, and feature rollouts are independent.

### Negative

- Two resource attribute models. `service.name` / `service.version` have to be
  set in both the Prometheus `build_info` collector and the OTel `resource.New`
  call. Minor duplication.
- No single-signal exemplar linking (Prometheus exemplars → OTel traces) out of
  the box. The OTel-native story here is better. Revisit if we want exemplars.
- Engineers joining the project learn two SDKs instead of one.

### Neutral

- If OTel metrics become the obvious choice later, migrating is a localized
  change: the metrics live behind a `*Metrics` struct in
  `internal/observability/metrics.go`. Handlers call methods on it; they do not
  touch the SDK directly. The SDK swap is contained to that one file.

## Alternatives Considered

- **OTel SDK for both signals.** Unify on one SDK, configure a Prometheus
  exporter to serve `/metrics`. Rejected for Phase 1 because the extra layer
  (OTel meter → Prometheus exporter → scrape) buys us nothing the native
  Prometheus client doesn't already provide, and adds a mature-but-younger SDK
  to the hot path.
- **Prometheus client for both signals.** There is no meaningful tracing story
  in `prometheus/client_golang`. Not a real option.
- **Push metrics via OTLP to a collector that fans out to Prometheus.** Adds a
  collector dependency webhookd would have to coordinate with for every metric
  change. Pull-based scraping remains the Prometheus-native path and the one our
  platform is already wired for.

## References

- DESIGN-0001 §Observability, §Non-Goals ("Custom OTel metrics… Phase 3 work").
- `prometheus/client_golang`: <https://github.com/prometheus/client_golang>
- OpenTelemetry Go SDK: <https://github.com/open-telemetry/opentelemetry-go>
- `otelhttp` contrib instrumentation:
  <https://github.com/open-telemetry/opentelemetry-go-contrib/tree/main/instrumentation/net/http/otelhttp>
