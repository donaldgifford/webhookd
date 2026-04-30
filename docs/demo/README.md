# webhookd Provider × Backend — Clean-Room Demo

A from-scratch walkthrough that builds the bare bones of the
[DESIGN-0004](../design/0004-multi-tenant-provider-x-backend-architecture.md)
architecture: a multi-tenant webhook receiver that pairs one **Provider**
(JSM) with one **Backend** (Kubernetes CR) per instance, routed by HCL2
config, instrumented with slog + OpenTelemetry + Prometheus.

**Goal:** prove the architecture before the production refactor lands.
Read top to bottom, optionally code along, smoke-test at the end.

## Reading order

| Phase | File | What you'll build |
|------:|------|-------------------|
| —     | [00-overview.md](00-overview.md) | Architecture, repo layout, deliverables |
| 1     | [01-bootstrap.md](01-bootstrap.md) | Module init, dependency choices, skeleton dirs |
| 2     | [02-config.md](02-config.md) | HCL2 config schema + typed loader |
| 3     | [03-registry.md](03-registry.md) | `Provider` / `Backend` interfaces, `Registry`, `BackendRequest` |
| 4     | [04-jsm-provider.md](04-jsm-provider.md) | JSM provider package (parse, sign-verify, build request, response) |
| 5     | [05-k8s-backend.md](05-k8s-backend.md) | Kubernetes backend (SSA apply + watch on a demo CRD) |
| 6     | [06-observability.md](06-observability.md) | slog handler, Prometheus registry, OTel tracer |
| 7     | [07-http.md](07-http.md) | Admin mux, server, signature middleware, rate limiter |
| 8     | [08-dispatcher.md](08-dispatcher.md) | Multi-tenant routing, idempotency tracker, response shaping |
| 9     | [09-main.md](09-main.md) | `main.go` wiring + graceful shutdown |
| 10    | [10-local-stack.md](10-local-stack.md) | docker-compose for OTel collector + Prometheus + Jaeger |
| 11    | [11-image-build.md](11-image-build.md) | Production-shaped Docker build via `docker buildx bake` |
| 12    | [12-kustomize.md](12-kustomize.md) | Minimal kustomize deployment to a kind cluster |
| 13    | [13-smoke-test.md](13-smoke-test.md) | End-to-end signed payload → CR → response |

## Prerequisites

| Tool | Why |
|------|-----|
| Go 1.26+ | The host language |
| Docker (with buildx) | docker-compose dev stack + image builds |
| kind | Local Kubernetes for the K8s backend |
| kubectl | Talk to the cluster |
| just | Task runner — recipes live in [`justfile`](justfile) |
| openssl | Sign demo payloads with HMAC-SHA256 |
| jq | Pretty-print JSON responses |

The [`justfile`](justfile) wraps every demo task. After the walkthrough
your loop will be:

```bash
just kind-up        # create kind cluster + apply demo CRD
just dev-stack      # docker-compose: otel-collector, prometheus, jaeger
just mock-operator  # tiny goroutine that flips Ready=True on demo CRs
just run            # webhookd-demo binary, native, against kind
just send-jsm       # POST a signed JSM payload, see 200 + traces + metrics
```

## What's intentionally out of scope

- **Async backends / callback pattern** — DESIGN-0004 Phase 4/5 territory; the demo only exercises the synchronous path so the architecture stays legible.
- **Hot reload** — config is read once at startup. Restart the binary.
- **Tests** — one example table-driven test per integration package is included to demonstrate the pattern; full coverage is out of scope.
- **Production hardening** — graceful drain, leak detection, license headers, lint config, fuzz targets — referenced where relevant, not implemented.
- **Multiple instances of the same provider/backend type** — the demo ships one JSM × K8s instance. Adding a second is one HCL block.

## Authority hierarchy

This demo **implements** DESIGN-0004 against the binding decisions in
ADR-0006 (sync response), ADR-0008 (callback over queues, deferred to
Phase 4/5), ADR-0009 (HCL2), ADR-0010 (static `init()` registration).
If the demo and a binding ADR diverge, the ADR wins.
