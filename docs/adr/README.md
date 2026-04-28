# Architecture Decision Records (ADRs)

This directory contains Architecture Decision Records documenting significant
technical decisions.

## What are ADRs?

ADRs document **technical implementation decisions** for specific architectural
components. Each ADR focuses on a single decision and includes:

- **Context**: The problem or constraint that led to this decision
- **Decision**: What was chosen and why
- **Consequences**: Trade-offs, pros, and cons
- **Alternatives**: Other options that were considered

## Creating a New ADR

```bash
docz create adr "Your ADR Title"
```

## ADR Status

- **Proposed**: Under discussion, not yet approved
- **Accepted**: Approved and being implemented or already implemented
- **Deprecated**: No longer relevant or superseded
- **Superseded by ADR-XXXX**: Replaced by another ADR

<!-- BEGIN DOCZ AUTO-GENERATED -->
## All ADRs

| ID | Title | Status | Date | Author | Link |
|----|-------|--------|------|--------|------|
| ADR-0001 | Use stdlib net/http ServeMux for HTTP routing | Accepted | 2026-04-24 | Donald Gifford | [0001-use-stdlib-nethttp-servemux-for-http-routing.md](0001-use-stdlib-nethttp-servemux-for-http-routing.md) |
| ADR-0002 | Prometheus for metrics, OpenTelemetry for traces | Accepted | 2026-04-24 | Donald Gifford | [0002-prometheus-for-metrics-opentelemetry-for-traces.md](0002-prometheus-for-metrics-opentelemetry-for-traces.md) |
| ADR-0003 | Environment-variable-only configuration | Accepted | 2026-04-24 | Donald Gifford | [0003-environment-variable-only-configuration.md](0003-environment-variable-only-configuration.md) |
| ADR-0004 | controller-runtime typed client for Kubernetes access | Accepted | 2026-04-24 | Donald Gifford | [0004-controller-runtime-typed-client-for-kubernetes-access.md](0004-controller-runtime-typed-client-for-kubernetes-access.md) |
| ADR-0005 | Server-Side Apply for custom resource reconciliation | Accepted | 2026-04-24 | Donald Gifford | [0005-server-side-apply-for-custom-resource-reconciliation.md](0005-server-side-apply-for-custom-resource-reconciliation.md) |
| ADR-0006 | Synchronous response contract for webhook providers | Accepted | 2026-04-24 | Donald Gifford | [0006-synchronous-response-contract-for-webhook-providers.md](0006-synchronous-response-contract-for-webhook-providers.md) |
| ADR-0007 | Trace context propagation via CR annotation | Accepted | 2026-04-28 | Donald Gifford | [0007-trace-context-propagation-via-cr-annotation.md](0007-trace-context-propagation-via-cr-annotation.md) |
<!-- END DOCZ AUTO-GENERATED -->
