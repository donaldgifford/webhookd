---
id: ADR-0007
title: "Trace context propagation via CR annotation"
status: Accepted
author: Donald Gifford
created: 2026-04-28
---

<!-- markdownlint-disable-file MD025 MD041 -->

# 0007. Trace context propagation via CR annotation

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

Phase 2 introduces a multi-process trace span: the JSM webhook hits
webhookd, which SSA-applies a `SAMLGroupMapping` CR, which the Wiz
operator reconciles by talking to the Wiz API. We want a single
distributed trace in Tempo that stitches these together — without
which, a Wiz failure will surface as an isolated operator error with
no path back to the originating JSM ticket.

W3C Trace Context handles HTTP-to-HTTP propagation via the
`traceparent` header, and that's how webhookd already correlates the
inbound JSM request with its outbound K8s API calls (via
`otelhttp.NewTransport`). But the operator's reconcile is not
HTTP-driven; it's triggered by the controller-runtime cache reacting
to a CR change. There's no header to carry the trace context across
the K8s apiserver boundary.

## Decision

webhookd writes the active span's W3C trace ID (the 32-hex-character
form, e.g. `0af7651916cd43dd8448eb211c80319c`) onto every CR it
applies, as the value of the `webhookd.io/trace-id` annotation. The
operator's reconciler reads this annotation, builds a remote-parent
`trace.SpanContext`, and starts the reconcile span with
`trace.WithLinks` (or `WithRemoteSpanContext` when a strict parent
relationship is desired).

Concretely:

- Annotation key: `webhookd.io/trace-id`.
- Annotation value: the lowercase 32-hex-character W3C trace-id with
  no surrounding whitespace, no `00-` prefix, no `traceparent`
  envelope. Just the trace-id.
- webhookd writes the annotation only when `trace.SpanFromContext`
  returns a valid `SpanContext`. The empty-tracing case (no exporter
  configured, sampling decided false at root) leaves the annotation
  absent rather than blank — operators dashboarding on the presence
  of the key see consistent semantics.
- The operator MUST NOT depend on this annotation for correctness.
  The annotation is observability metadata, not a control plane
  signal — webhookd may upgrade to a richer encoding (e.g., the full
  `traceparent` value) in a future ADR.

## Consequences

### Positive

- Tempo can render a single trace spanning the JSM → webhookd →
  operator → Wiz path. SREs investigating a failed Wiz provisioning
  ticket can pivot from the trace ID in webhookd's response body
  directly to the operator's reconcile span.
- The contract is one annotation key, one string value. Cheap for the
  operator to implement and forward-compatible — webhookd can ship a
  richer encoding in a separate annotation without breaking existing
  operator builds.
- Decoupled lifecycle: webhookd and the operator can ship trace
  changes independently. The operator's existing dependency on
  webhookd's CR shape doesn't grow.

### Negative

- The annotation is one-way: the operator's reconcile span isn't
  visible in the JSM response body, so the JSM automation rule
  comments only carry webhookd's trace-id. Operators investigating
  via JSM will need a Tempo lookup to find the operator's spans.
- Trace IDs are not exposed in `kubectl describe` output by default;
  `kubectl get -o yaml` does include them. Acceptable until the
  Phase 8 deployment guide documents the lookup pattern.
- A misconfigured (or absent) sampler at the root span produces an
  empty annotation. The operator must handle this gracefully —
  documented in the contract above.

### Neutral

- Annotation key is `webhookd.io/`-namespaced, matching the project's
  existing label/annotation prefix convention. The operator owns the
  `wiz.webhookd.io/` namespace; webhookd never writes there.
- Phase 2 only ships the writer side. The operator-side reader lives
  in `github.com/donaldgifford/wiz-operator` and is referenced from
  IMPL-0002 §Cross-doc follow-ups.

## Alternatives Considered

**1. W3C `traceparent` annotation.** Storing the full
`00-<traceid>-<spanid>-<flags>` value would let the operator stitch
its reconcile span as a true child rather than a link. Rejected for
Phase 2: webhookd's caller is HTTP, the operator's caller is the
informer cache, and there's no causal ordering between webhookd's
exit span and the operator's reconcile entry. A "link" is the
honest semantic. We can always upgrade to `traceparent` later
without breaking the simpler-key consumers.

**2. CR `status.observabilty.traceId`.** Putting trace metadata in
`.status` would arguably be more discoverable in `kubectl describe`.
Rejected because `.status` is the operator's namespace by K8s
convention — webhookd should not write there. Annotations are the
right tool for "metadata I want to follow the object through its
lifecycle."

**3. Separate sidecar API.** A long-running sidecar that exposes a
"trace ID for ticket X" lookup endpoint. Rejected as Phase 2
overkill. The annotation is one line of write code; a sidecar is a
new deploy artifact.

## References

- DESIGN-0002 §Observability — motivates the cross-process trace.
- IMPL-0002 §Phase 7 — task list for landing the writer side.
- W3C Trace Context: <https://www.w3.org/TR/trace-context/>
- ADR-0002 — Prometheus for metrics, OpenTelemetry for traces.
