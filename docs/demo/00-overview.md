# 00. Overview

## What we're building

A binary called `webhookd-demo` that:

1. Listens on `:8080` for `POST /{provider_type}/{webhook_id}` requests.
2. Looks up a configured **instance** by `webhook_id`.
3. Hands the request body to the configured **Provider** (e.g. `jsm`)
   for signature verification + parsing into a typed `BackendRequest`.
4. Hands the `BackendRequest` to the configured **Backend** (e.g. `k8s`)
   for execution (SSA apply + Watch).
5. Builds a Provider-shaped response from the Backend's `ExecResult` and
   writes it synchronously.
6. Exposes Prometheus metrics + OTel traces + structured logs throughout.

The whole point is to validate the **decoupling**: a Provider doesn't
know which Backend will run its request; a Backend doesn't know which
Provider produced it. The dispatcher handles routing, idempotency,
response shaping, and observability cross-cutting in one place.

## Architecture diagram

```
                  HTTP request
                     │
                     ▼
       ┌────────────────────────────────┐
       │  /{provider_type}/{webhook_id} │   stdlib net/http ServeMux
       └────────────────────────────────┘
                     │
                     ▼
       ┌──────────────────────────────────────────┐
       │  Dispatcher                              │
       │   ├── lookup instance by webhook_id      │
       │   ├── verify signature (Provider)        │
       │   ├── compute idempotency key (Provider) │
       │   ├── deduplicate (IdempotencyTracker)   │
       │   ├── parse → BackendRequest (Provider)  │
       │   ├── execute (Backend)                  │
       │   ├── shape response (Provider)          │
       │   └── emit traces / metrics / logs       │
       └──────────────────────────────────────────┘
            │           │            │
            ▼           ▼            ▼
       ┌────────┐  ┌─────────┐  ┌──────────┐
       │ JSM    │  │ K8s     │  │  ...     │
       │ provider│  │ backend │  │ future   │
       └────────┘  └─────────┘  └──────────┘
                          │
                          ▼
                    Kubernetes API
                    (SSA + Watch)
```

## Repo layout (the project you'll build)

```
webhookd-demo/
├── cmd/
│   ├── webhookd-demo/main.go        # entry point + wiring
│   └── mock-operator/main.go        # flips Ready=True on demo CRs
├── internal/
│   ├── api/v1alpha1/                # demo CRD types (handwritten)
│   │   ├── groupversion_info.go
│   │   ├── types.go
│   │   └── zz_generated.deepcopy.go
│   ├── config/                      # HCL2 schema + loader
│   │   └── config.go
│   ├── observability/               # slog + Prom + OTel
│   │   ├── logging.go
│   │   ├── metrics.go
│   │   └── tracing.go
│   ├── httpx/                       # admin mux, server, middleware
│   │   ├── admin.go
│   │   ├── server.go
│   │   └── middleware.go
│   ├── webhook/                     # registry + dispatcher + idempotency
│   │   ├── types.go
│   │   ├── registry.go
│   │   ├── dispatcher.go
│   │   ├── idempotency.go
│   │   └── signature.go
│   ├── k8s/                         # scheme + client
│   │   └── clients.go
│   └── integrations/
│       ├── jsm/                     # Provider impl
│       │   ├── config.go
│       │   ├── provider.go
│       │   ├── decode.go
│       │   ├── request.go
│       │   ├── response.go
│       │   ├── init.go
│       │   └── provider_test.go
│       └── k8sbackend/              # Backend impl
│           ├── config.go
│           ├── backend.go
│           ├── apply.go
│           ├── watch.go
│           └── init.go
├── go.mod
└── go.sum
```

## Dependencies

Stdlib by default. The deps that earn their seat:

| Module | Why |
|--------|-----|
| `github.com/hashicorp/hcl/v2` | Typed config decoding (ADR-0009) |
| `go.opentelemetry.io/otel` (+ exporters) | Distributed tracing |
| `go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc` | OTLP/gRPC trace export |
| `github.com/prometheus/client_golang` | Metrics registry + handler |
| `golang.org/x/time/rate` | Token-bucket rate limiter |
| `sigs.k8s.io/controller-runtime` | Typed K8s client + SSA |
| `k8s.io/api`, `k8s.io/apimachinery`, `k8s.io/client-go` | Pinned to controller-runtime's go.mod |

That's it. No web framework, no logging library (slog is stdlib in
Go 1.21+), no router (Go 1.22+ ServeMux supports path values).

## Deliverables checklist

By the end of the walkthrough you will have:

- [ ] A working `webhookd-demo` binary that accepts signed JSM payloads
- [ ] A demo CRD (`WebhookMapping`) installed in a kind cluster
- [ ] A mock operator that flips `Ready=True` so the watch step succeeds
- [ ] Prometheus scraping `:9090/metrics` showing `webhookd_*` metrics
- [ ] Jaeger displaying traces with spans across HTTP → Provider → Backend → K8s
- [ ] A multi-arch Docker image built via `docker buildx bake`
- [ ] A kustomize-deployable variant running in the kind cluster
- [ ] A signed `curl` smoke test returning a `200 OK` JSM-shaped response

## Conventions used throughout

- **Module path:** `github.com/example/webhookd-demo`. Replace with your own when copying.
- **Package layout:** `cmd/` for entry points, `internal/` for everything not exported. `pkg/` is **not** used — the demo doesn't expose a public API surface.
- **Errors:** wrap with `%w`, lowercase strings, no `failed to`. `fmt.Errorf("decode body: %w", err)`.
- **Context:** `context.Context` is the first arg of every function that does I/O. HTTP handlers pull from `r.Context()`.
- **Naming:** no stutter (`webhook.Registry` not `webhook.WebhookRegistry`). Constructors return concrete types.
- **Receivers:** pointer receivers when the type contains a mutex, sync.Map, or other state. Otherwise the simpler value receiver.
- **Tests:** table-driven where there are multiple cases. `_test.go` co-located with source. The demo includes one example test in the JSM package — apply the same pattern elsewhere.

## What we'll skip and where to find it

| Skipped | Lives in |
|---------|----------|
| SPDX license headers | Production webhookd's `licenses-header.txt` + `goheader` lint |
| Full lint config (gocyclo, gocognit, funlen, etc.) | Production webhookd's `.golangci.yml` |
| `mise.toml` tool pinning | Production webhookd's `mise.toml` |
| Per-provider rate limiting refinement | DESIGN-0004 §HTTP framework |
| Async backends + callback pattern | DESIGN-0004 §AsyncBackend, ADR-0008 |
| Hot reload | DESIGN-0004 §Migration §Hot reload |
| Multi-file HCL directory loading | ADR-0009 — single file is fine for the demo |

Onward to [01-bootstrap.md](01-bootstrap.md).
