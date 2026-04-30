---
id: RFC-0001
title: "Generalize webhookd to Provider x Backend with Multi-Tenant Routing"
status: Draft
author: Donald Gifford
created: 2026-04-30
---
<!-- markdownlint-disable-file MD025 MD041 -->

# RFC 0001: Generalize webhookd to Provider x Backend with Multi-Tenant Routing

**Status:** Draft
**Author:** Donald Gifford
**Date:** 2026-04-30

<!--toc:start-->
- [Summary](#summary)
- [Problem Statement](#problem-statement)
- [Proposed Solution](#proposed-solution)
- [Design](#design)
  - [Provider interface](#provider-interface)
  - [Backend interface](#backend-interface)
  - [BackendRequest: opaque payload between Provider and Backend](#backendrequest-opaque-payload-between-provider-and-backend)
  - [Webhook instances and routing](#webhook-instances-and-routing)
  - [Configuration format](#configuration-format)
  - [Static integration registration](#static-integration-registration)
  - [Per-provider idempotency keys](#per-provider-idempotency-keys)
  - [Long-running work via provider callbacks](#long-running-work-via-provider-callbacks)
  - [Package layout](#package-layout)
- [Alternatives Considered](#alternatives-considered)
  - [Argo Events as the platform (rejected)](#argo-events-as-the-platform-rejected)
  - [Stay single-tenant per provider (rejected)](#stay-single-tenant-per-provider-rejected)
  - [Closed Action union with switch dispatch (rejected — current shape)](#closed-action-union-with-switch-dispatch-rejected--current-shape)
  - [Go plugins for integrations (rejected)](#go-plugins-for-integrations-rejected)
  - [Durable queue (NATS / Redis / Postgres outbox) for backend dispatch (deferred)](#durable-queue-nats--redis--postgres-outbox-for-backend-dispatch-deferred)
  - [Pipeline / eventbus / fan-out as first-class composition (rejected)](#pipeline--eventbus--fan-out-as-first-class-composition-rejected)
  - [YAML or CRDs for configuration (rejected / deferred)](#yaml-or-crds-for-configuration-rejected--deferred)
- [Implementation Phases](#implementation-phases)
  - [Phase 0 — Move existing code into the new package layout](#phase-0--move-existing-code-into-the-new-package-layout)
  - [Phase 1 — Backend interface + BackendRequest + idempotency](#phase-1--backend-interface--backendrequest--idempotency)
  - [Phase 2 — Multi-tenant: HCL2 config + /{provider}/{id} routing](#phase-2--multi-tenant-hcl2-config--providerid-routing)
  - [Phase 3 — Second integration: AWS Backend or GitHub Provider+Backend](#phase-3--second-integration-aws-backend-or-github-providerbackend)
  - [Phase 4 — Long-running work: callback-pattern shape on Backend](#phase-4--long-running-work-callback-pattern-shape-on-backend)
  - [Phase 5 — (Genuinely deferred, possibly never) Durable queue + state](#phase-5--genuinely-deferred-possibly-never-durable-queue--state)
- [Risks and Mitigations](#risks-and-mitigations)
- [Success Criteria](#success-criteria)
- [Open Questions](#open-questions)
- [References](#references)
  - [Decisions extracted from this RFC into standalone ADRs](#decisions-extracted-from-this-rfc-into-standalone-adrs)
  - [Source investigations](#source-investigations)
  - [Designs and implementations carrying forward](#designs-and-implementations-carrying-forward)
  - [ADRs that constrain or carry forward into this RFC](#adrs-that-constrain-or-carry-forward-into-this-rfc)
  - [Prior art](#prior-art)
<!--toc:end-->

## Summary

webhookd today is shaped around a single Provider (JSM) wired to a single Backend (Kubernetes Server-Side Apply of a `SAMLGroupMapping` CR). This RFC proposes generalizing the architecture so that arbitrary input integrations (jsm, gh, slack, …) can be paired with arbitrary output systems (k8s, aws, http, …) at config time, with multi-tenant routing on `/{provider_type}/{webhook_id}`. The synchronous response contract (ADR-0006) and the existing observability + signing substrate (DESIGN-0001) survive unchanged. The refactor is mostly mechanical — extract a `Backend` interface, replace the closed `Action` union with an open `BackendRequest`, add an HCL2 config layer for instance definitions — and explicitly defers durable-queue infrastructure in favor of provider-callback patterns for long-running work.

## Problem Statement

The current architecture has three load-bearing assumptions baked in that don't survive the project's stated end-goal of being a general webhook service:

1. **One Provider per binary, one Backend per Provider.** The `Action` type union (`internal/webhook/action.go`) is a closed sentinel-method type; adding a new variant requires editing the central package and the executor's switch arm. The `Executor` (`internal/webhook/executor.go`) is K8s-only — name, dependencies, and constants (`crKindLabel = "SAMLGroupMapping"`) all hardcode the JSM→K8s pairing. Cross-cutting concerns are stuck inside it.
2. **Single-tenant routing.** `POST /webhook/{provider}` allows exactly one instance per provider type. Two JSM tenants pointing at different K8s clusters is not expressible.
3. **Env-driven config can't represent N×M instances.** `internal/config/config.go` (463 lines) pre-bakes `JSMConfig` and `CRConfig` structs and gates them behind `WEBHOOK_PROVIDERS=jsm`. There is no place to put a list of `(provider, backend, provider-config, backend-config)` tuples.

The user-visible end-goal is multi-instance, multi-vendor: many JSM workflows pointing at independent K8s/AWS/whatever backends, plus future providers (GitHub, Slack) that themselves may also act as backends. The current shape blocks this, and the longer we wait, the more we accumulate JSM-shaped assumptions in the substrate that survive into v1 of the new shape.

The investigation supporting this proposal — INV-0001 — found that the current `Provider` interface is already 80% of the proposed shape. The cost of the refactor is low; the cost of *not* doing it before the second integration lands is much higher.

## Proposed Solution

Introduce a clear input/output split:

- **`Provider`** — input integration. One implementation per vendor that *sends* webhooks to webhookd (jsm, github, slack, gitlab, …). Pure functions: receive bytes + headers, verify signature, parse, return a `BackendRequest`.
- **`Backend`** — output integration. One implementation per downstream system webhookd *delivers to* (k8s, aws, http, …). Side-effectful: takes a `BackendRequest`, performs the work, returns an `ExecResult`.
- **`BackendRequest`** — opaque payload that flows between them. Replaces the closed `Action` union with an open interface; each (Provider, Backend) pair agrees on a concrete type at wiring time.
- **Webhook instances** — the unit of multi-tenancy. Each instance binds one Provider to one Backend with their respective configurations. Routed via `/{provider_type}/{webhook_id}` where `webhook_id` is an opaque random ID generated at config time.
- **HCL2 config file (or directory)** — a list of `instance "ID" { provider "type" {...} backend "type" {...} }` blocks, loaded at startup. Per-integration typed decoding via `gohcl.DecodeBody` — no JSON-Schema fragmentation. Hard cutover from env vars (no `--legacy-env` shim — nothing's live yet).
- **Static, build-time integration registration** — each integration package self-registers via `init()` (or is explicitly imported in main); no Go plugins.
- **Per-provider idempotency keys** — each Provider contributes a pure `IdempotencyKey(payload) string`, the dispatcher gates duplicate processing via a pod-local `sync.Map[(provider, key)]`. Distinct from HMAC nonce dedup — these address logical-event duplication (JSM retried because we were slow), not security.
- **Provider-callback pattern for long-running work** — when a Backend's work won't fit in the HTTP timeout budget, respond `202 Accepted` and POST the result to the originator's callback URL. No durable queue infrastructure required for the cases we have (or expect).

The scope of this RFC is the high-level shape. Concrete interface signatures, file layout, and config schema are deferred to DESIGN-0004 (the design doc that follows this RFC).

## Design

### Provider interface

```go
// Provider is the input seam — one per vendor that sends webhooks.
type Provider interface {
    // Type returns the URL path segment for this provider type (e.g. "jsm").
    // Used for routing, metrics labels, and config parsing. Stable.
    Type() string

    // VerifySignature validates request authenticity using the provider's
    // own conventions. Must be timing-safe.
    VerifySignature(r *http.Request, body []byte, cfg ProviderConfig) error

    // Handle is pure: bytes + config in, BackendRequest out. No I/O against
    // K8s, the network, or any side-effectful system. Side effects belong
    // to the matching Backend.
    Handle(ctx context.Context, body []byte, cfg ProviderConfig) (BackendRequest, error)

    // IdempotencyKey returns a provider-specific dedup key for the given
    // payload (e.g. JSM ticket key, GitHub delivery-id, Slack event-id).
    // Empty string disables idempotency for this request.
    IdempotencyKey(body []byte) (string, error)

    // ResponseBuilder returns the per-provider response shape (e.g. JSM
    // wants `crName` + `traceId` for ticket-comment automation).
    ResponseBuilder() ResponseBuilder
}
```

`ProviderConfig` is a per-instance opaque value the Provider knows how to consume — typically a typed struct produced by the Provider's own `gohcl.DecodeBody` call against the HCL2 instance block. The dispatcher does not introspect it.

### Backend interface

```go
// Backend is the output seam — one per downstream system webhookd dispatches to.
type Backend interface {
    // Type returns the backend identifier ("k8s", "aws", "http", …). Used
    // for config parsing and metrics labels. Stable.
    Type() string

    // Execute performs the side-effectful work for the given BackendRequest.
    // The returned ExecResult carries everything the dispatcher needs to
    // shape the HTTP response (kind, reason, optional resource identity).
    Execute(ctx context.Context, req BackendRequest, cfg BackendConfig) ExecResult
}
```

The synchronous-only signature here is intentional for v1. The Phase-4 long-running-work shape (callback pattern) is layered on top via an additional interface (`AsyncBackend` or similar), without disturbing this base contract; see [Phase 4](#phase-4--long-running-work-callback-pattern-shape-on-backend).

### BackendRequest: opaque payload between Provider and Backend

```go
// BackendRequest is what flows from Provider.Handle to Backend.Execute.
// Each (Provider, Backend) pair agrees on a concrete type at wiring time;
// the dispatcher just plumbs it through.
type BackendRequest interface {
    // BackendType returns the matching Backend.Type() this request requires.
    // Allows the dispatcher to fail fast if config wires a provider that
    // produced a request type the bound backend can't handle.
    BackendType() string
}
```

Concrete types live in the integration packages (e.g. `internal/integrations/k8s.ApplyCRRequest`, `internal/integrations/aws.PublishSNSRequest`). The dispatcher never type-asserts on them.

### Webhook instances and routing

Each instance is a runtime tuple:

```go
type Instance struct {
    ID            string          // opaque, e.g. "abc123def456"
    Provider      Provider        // resolved from registry by config Type
    Backend       Backend         // resolved from registry by config Type
    ProviderConfig ProviderConfig // typed, provider-decoded
    BackendConfig  BackendConfig  // typed, backend-decoded
    IdempotencyTTL time.Duration  // default 5m; per-instance override allowed
}
```

Routing:

- `POST /{provider_type}/{webhook_id}` — public, signed, rate-limited.
- Dispatcher lookup: `instances[(provider_type, webhook_id)] → Instance` in O(1).
- Mismatch (unknown provider type, unknown ID, or signature fails per the instance's secret) → `404` for routing failures, `401` for signature failures. Same shape as today.
- Provider type goes in the URL because: ingress can route by path prefix; logs are greppable per provider; metrics labels are stable; misconfigs fail fast (404 at routing time, not signature-verify time).

`webhook_id` is opaque — random 12-char base32, generated at config time. No tenant names, no human-readable slugs (they leak into access logs, Prometheus labels, and span attributes).

### Configuration format

HCL2, per **ADR-0009**. Supersedes ADR-0003 (env-var-only) when this RFC lands. Single file or directory load (HCL2's parser merges all `*.hcl` files in a directory). Per-integration typed decoding via `gohcl.DecodeBody` — Go struct + `hcl:""` tags is the schema, no separate JSON Schema fragments. Reference shape (full rationale in the ADR):

```hcl
# webhookd.hcl — loaded at startup. Hot-reload deferred.

instance "abc123def456" {
  provider "jsm" {
    trigger_status = "Approved"
    fields {
      provider_group_id = "customfield_10001"
      role              = "customfield_10002"
      project           = "customfield_10003"
    }
    signing {
      secret_env       = "WEBHOOK_JSM_TENANT_A_SECRET"
      signature_header = "X-Hub-Signature-256"
      skew             = "5m"
    }
  }
  backend "k8s" {
    kubeconfig_env       = "KUBECONFIG_TENANT_A"
    namespace            = "wiz-operator"
    identity_provider_id = "tenant-a-idp"
    sync_timeout         = "20s"
  }
  idempotency_ttl = "5m"
}

instance "7xkqp3l9zwer" {
  provider "github" {
    events = ["pull_request", "check_run"]
    signing { secret_env = "WEBHOOK_GH_ORG_X_SECRET" }
  }
  backend "aws" {
    region    = "us-west-2"
    event_bus = "prod-events"
  }
}
```

Secrets are *always* referenced by env-var name (`secret_env`), never inline. The chart renders HCL into a `ConfigMap`; secret env vars stay env-mapped via `existingSecret`, mirroring DESIGN-0003. CRDs are deferred per ADR-0009.

### Static integration registration

Static, build-time imports per **ADR-0010**. No Go plugins. Each integration package self-registers via `init()` with an explicit `webhook.NewRegistry()` opt-out for tests:

```go
// cmd/webhookd/main.go — every integration is a build-time import.
import (
    _ "github.com/donaldgifford/webhookd/internal/integrations/jsm"
    _ "github.com/donaldgifford/webhookd/internal/integrations/github"
    _ "github.com/donaldgifford/webhookd/internal/integrations/k8s"
    _ "github.com/donaldgifford/webhookd/internal/integrations/aws"
)

// internal/integrations/github/init.go — vendor that ships both interfaces
func init() {
    webhook.RegisterProvider(&Provider{})  // github.Provider — distinct type
    webhook.RegisterBackend(&Backend{})    // github.Backend  — distinct type
}
```

GitHub is the explicit pressure-test for "one vendor ships both interfaces in one package as two distinct types": they share configuration and authentication state internally but implement disjoint interfaces externally. Full rationale (plugin rejection, ordering, test isolation) in the ADR.

### Per-provider idempotency keys

Distinct from HMAC nonce dedup. Three layered mechanisms:

| Mechanism | Protects against | Key | Storage |
|---|---|---|---|
| Timestamp skew | Replay of an old signed request after a long delay | Implicit (timestamp value) | Stateless |
| Nonce dedup *(optional)* | Replay of a signed request *within* the skew window | HMAC signature digest | Pod-local LRU or Redis |
| Idempotency keys | Duplicate processing of the same logical event | Provider-derived (JSM ticket key, GH delivery-id, Slack event-id) | Pod-local `sync.Map` with TTL |

Implementation:

```go
key, err := provider.IdempotencyKey(body)
if err != nil { /* malformed → 400 */ }
if key != "" {
    if !inflight.Acquire(provider.Type(), key, instance.IdempotencyTTL) {
        // Same logical event already in-flight or recently completed; respond
        // with the cached previous result (or 200-noop with a "duplicate" reason).
        return cachedResultOrNoop()
    }
    defer inflight.Release(provider.Type(), key)
}
```

Pod-local is honest: cross-pod idempotency would require shared storage we're not pulling in. The common failure mode (a chatty caller retrying within seconds) is handled by L7 LB affinity hitting the same pod. The rare cross-pod case produces duplicate work — same outcome as today.

The `sync.Map` requires LRU eviction or TTL-based GC to bound memory. Detail-level for DESIGN-0004.

### Long-running work via provider callbacks

Decision and full rationale in **ADR-0008**. The shape this RFC commits to:

- Synchronous `Backend.Execute` is v1's default; works for the current K8s SSA-and-watch backend that finishes in seconds.
- Backends that can exceed an HTTP-timeout budget opt into `AsyncBackend` (extends `Backend`), returning a `PendingToken`; the dispatcher responds `202 Accepted` and runs the work in a goroutine.
- On completion, the goroutine invokes the Provider's `Callback` helper, which POSTs the result to the originator's callback URL with an HMAC-signed body.
- Pod-crash recovery is three-layered: provider-side callback idempotency (default), `webhookd.io/callback-fired-at` annotation on the K8s target (added if a specific provider misbehaves), durable queue (Phase 5, genuinely deferred). See ADR-0008 for the full enumeration.

```go
// Optional extension. Backends opt in by implementing this alongside Backend.
type AsyncBackend interface {
    Backend
    ExecuteAsync(ctx context.Context, req BackendRequest, cfg BackendConfig) (PendingToken, error)
    Callback(ctx context.Context, token PendingToken) ExecResult
}
```

Concrete signatures are deferred to DESIGN-0004; this RFC commits to the shape, not the byte-level types.

### Package layout

```
internal/
├── webhook/
│   ├── dispatcher.go         # routing + plumbing only — no integration logic
│   ├── provider.go           # Provider interface
│   ├── backend.go            # Backend, AsyncBackend interfaces
│   ├── request.go            # BackendRequest, ExecResult, ResultKind
│   ├── registry.go           # RegisterProvider, RegisterBackend
│   ├── instance.go           # Instance type + (provider, id) lookup
│   └── idempotency.go        # sync.Map-backed in-flight tracker
├── integrations/
│   ├── jsm/                  # Provider only (input)
│   ├── github/               # Provider AND Backend (distinct types in one package)
│   ├── slack/                # Provider only (input)
│   ├── k8s/                  # Backend only (output)
│   ├── aws/                  # Backend only (output)
│   └── http/                 # Backend only (output) — generic HTTP forwarder
└── config/
    └── config.go             # HCL2 loader; produces []Instance
```

Old `internal/webhook/jsm/`, `internal/webhook/executor.go`, and `internal/webhook/wizapi/` get moved (Phase 0). Old env-driven `internal/config/` gets rewritten (Phase 2).

## Alternatives Considered

### Argo Events as the platform (rejected)

**See INV-0002 for the full evaluation.** Summary of why it doesn't fit:

The Argo Events shape — `EventBus → EventSource → Sensor → Trigger` — is async-by-design. The webhook EventSource returns `200 OK` as soon as it has published the event to the EventBus, *before* the Sensor's trigger executes. There is no built-in mechanism for the trigger's outcome to flow back into the original HTTP response.

That breaks ADR-0006's synchronous response contract, which is load-bearing for the existing JSM workflow: JSM's automation rule reads the HTTP response body to decide what to do next. With Argo Events as the receiver, that response is "received, will process," not "applied and Ready=True."

The four workarounds enumerated in INV-0002 each carry a meaningful cost:

- **JSM-side polling** — defeats the purpose; we'd build a status endpoint too.
- **Two-Sensor callback** — requires JSM-side automation redesign, which is a major scope change.
- **Custom proxy in front of Argo Events** — that proxy *is* webhookd, with worse isolation.
- **Argo Workflows trigger** — workflow engine for a one-step operation; wrong tool.

Argo Events is also missing HMAC-SHA256 verification on its generic `webhook` EventSource (only vendor-specific sources verify their own signatures). Closing that gap is itself ~the same engineering as building webhookd's signature path.

**The conclusion of INV-0002 carries forward to this RFC unchanged: continue building webhookd for synchronous flows; pin Argo Events as a candidate Backend for *future* fan-out / async use cases (Phase 5+ territory), not as a substitute for the platform.** That mapping is intentional — when the project eventually does need durable async fan-out, Argo Events is a strong candidate to integrate with rather than replace.

### Stay single-tenant per provider (rejected)

The status quo. Cost of *not* refactoring grows quadratically as integrations accumulate: every new provider/backend pair would require (a) a new `Action` variant in the central package, (b) a new arm in the executor switch, (c) a new env-var block in the config, (d) duplicated rate-limiting and metrics wiring. Three integrations in we'd be staring down a much harder rewrite. Better to do it now while the surface area is small.

### Closed Action union with switch dispatch (rejected — current shape)

The existing `Action` interface uses an unexported sentinel method to enforce a closed set of variants known to the executor. This made sense for Phase 2 (one provider, one variant); it stops scaling at two backends because every new variant requires editing the central `webhook` package and the executor's switch. Replacing the closed union with the open `BackendRequest` interface is the single biggest unlock in this RFC.

### Go plugins for integrations (rejected)

Decision and full rationale in **ADR-0010**. One-liner: Go's `plugin` package requires exact-version host/plugin alignment, traffics in `interface{}`, and doesn't add value for our release-disciplined deployment model. Static linking via build-time imports is the answer.

### Durable queue (NATS / Redis / Postgres outbox) for backend dispatch (deferred)

Decision and full rationale in **ADR-0008**. One-liner: the provider-callback pattern handles long-running work without new infrastructure for every workload on the roadmap. Durable queues are deferred to Phase 5 and only justified by specific failure modes (provider doesn't support inbound callbacks; fan-out with independent retries; compliance audit trail; callback-dedupe complexity outgrowing annotations). When that trigger fires, NATS JetStream is the favored default.

### Pipeline / eventbus / fan-out as first-class composition (rejected)

Pipeline-shaped composition (Provider → Filter → Transform → Backend, like n8n / Zapier) is a massive complexity bump for use cases we don't have. The pure `Provider.Handle(body) → BackendRequest` already lets you do filter+transform inside the Provider; pulling those into separate stages is YAGNI. Pass-through (Provider → Backend, direct) is the right shape for the workloads on the roadmap.

### YAML or CRDs for configuration (rejected / deferred)

Decision and full rationale in **ADR-0009**. One-liner: YAML was the original v1 choice in this RFC's earlier draft, but the per-integration JSON-Schema fragmentation cost flipped the decision to HCL2 during DESIGN-0004 review (HCL2's typed decoding via `gohcl.DecodeBody` makes the Go struct itself the schema, eliminating the fragmentation problem). CRDs remain deferred for the GitOps-per-instance use case.

## Implementation Phases

Each phase is independently reviewable + mergeable. Tests stay green throughout. No phase requires the next.

### Phase 0 — Move existing code into the new package layout

Pure rename, no behavior change.

- `internal/webhook/jsm/` → `internal/integrations/jsm/`
- `internal/webhook/executor.go` → `internal/integrations/k8s/backend.go`
- `internal/webhook/wizapi/` → `internal/integrations/k8s/wizapi/` (since it's now a K8s-backend implementation detail)
- All tests still pass at the new import paths.

Reviewable in one sitting.

### Phase 1 — Backend interface + BackendRequest + idempotency

- Define `Backend`, `AsyncBackend` interfaces in `internal/webhook/`.
- K8s backend now satisfies `Backend`. The closed `Action` union becomes the open `BackendRequest` interface; `ApplySAMLGroupMapping` becomes `k8s.ApplyCRRequest` in the integration package.
- Add `Provider.IdempotencyKey` to the interface, with JSM extracting from `payload.IssueKey()`.
- Dispatcher gains the in-flight tracker (`sync.Map` with TTL).
- Still single-tenant (one Provider, one Backend, env-driven config). Behavior visible to callers is unchanged.

### Phase 2 — Multi-tenant: HCL2 config + `/{provider}/{id}` routing

- New `internal/config/config.go` parses HCL2 (single file or directory) into the typed `Config` struct.
- Each integration gains a `Config` struct with `hcl:""` tags; `DecodeConfig` calls `gohcl.DecodeBody` against it.
- Routing changes to `/{provider_type}/{webhook_id}`.
- Helm chart updates: surface the HCL config via a `ConfigMap`; secrets are still env-mapped via `existingSecret`.
- **Hard cutover** — no env-var compat shim. Helm `values.yaml` schema breaks.
- Single-instance JSM users get a one-time migration: their existing env vars become a single HCL `instance "..." { ... }` block. Documented in CHANGELOG + runbook.

### Phase 3 — Second integration: AWS Backend or GitHub Provider+Backend

Pick one to validate the abstraction. Either works as a first second-integration; both eventually land.

- **AWS Backend** validates the "providers and backends are not 1:1 — JSM can target K8s *or* AWS" model. Likely synchronous (call AWS APIs, return result).
- **GitHub Provider + GitHub Backend** validates the "one vendor ships both interfaces in one package as two distinct types" model. Receive `pull_request.opened`, post a PR comment.

The phase succeeds if the abstraction holds with no `webhook` package edits — the integration ships entirely under `internal/integrations/<name>/` plus one import line in main.

### Phase 4 — Long-running work: callback-pattern shape on Backend

- Define `AsyncBackend` interface (synchronous `Backend` is the base, async is opt-in).
- Add the per-Provider callback-poster shape: each Provider that supports inbound callbacks (JSM, GitHub, Slack) ships a small `Callback(token, result)` helper that POSTs to the originator's callback URL with HMAC-signed body.
- Add `webhookd.io/callback-fired-at` annotation handling for K8s targets that benefit from second-level dedupe (Phase 4 doesn't *require* this — only adds it if a backend integration shows it needs it).
- No durable queue. Trust provider-side callback idempotency by default.

Touch only if a real backend on the roadmap needs it. Phase 4 is opt-in per integration.

### Phase 5 — (Genuinely deferred, possibly never) Durable queue + state

Triggered only by a documented failure mode the callback pattern + annotation can't cover. NATS JetStream is the favored option; Postgres outbox is the runner-up; Argo Events as a Backend is the third option for fan-out cases. Each comes with its own RFC at the time, scoped to the specific failure mode. This RFC explicitly does *not* commit to any of them.

## Risks and Mitigations

| Risk | Impact | Likelihood | Mitigation |
|------|--------|------------|------------|
| Multi-tenant routing exposes a new attack surface — wrong webhook ID + right secret could route to the wrong backend | Medium | Low | The dispatcher resolves Instance by `(provider_type, webhook_id)` *before* signature verification; signing secret is per-instance; mismatched (provider_type, webhook_id) → 404 not 401. No cross-instance leakage. |
| Per-provider idempotency `sync.Map` grows unbounded under chatty callers | Medium | Medium | TTL-based eviction (default 5 min), bounded LRU as backstop, per-instance cap (e.g., 10k entries). Detail-level for DESIGN-0004. |
| Hard cutover breaks anyone with a Helm install of the JSM-shaped chart | High (for them) | Low (nothing's live yet) | Document in CHANGELOG; ship a one-page migration runbook; chart `appVersion` major bump + clear breaking-change note. |
| Pod-crash mid-callback in Phase 4 leads to no JSM update (status hangs) | Medium | Low (Phase 4 is opt-in per integration) | Lean on provider-side callback idempotency (most providers are idempotent); add `callback-fired-at` annotation if a specific provider misbehaves; document the failure mode. |
| GitHub Provider+Backend in same package becomes a tangled-coupling temptation | Medium | Medium | Enforce distinct types (`github.Provider`, `github.Backend`) implementing disjoint interfaces; share only configuration + auth state; covered by code review. DESIGN-0004 will codify the pattern. |
| HCL2 contributors are unfamiliar with the syntax | Low | Medium | HCL2 primer in the runbook; integration package READMEs include a sample block; `webhookd config validate` surfaces structured `hcl.Diagnostics` errors with file/line/column on misconfig. |
| Static integration registration via `init()` makes test isolation harder | Low | Medium | Provide an `init()`-free registration path (`webhook.NewRegistry()` for tests); `init()` is a convenience for production wiring, not a requirement. |

## Success Criteria

This RFC is successful if:

1. **Phase 0** lands — code moves to the new layout, all 50+ helm-unittest cases and Go tests stay green. *No behavior change visible to users.*
2. **Phase 1** lands — K8s implementation satisfies `Backend`, `BackendRequest` is open, idempotency keys gate duplicates. The closed `Action` union is gone from the codebase.
3. **Phase 2** lands — multi-tenant routing works end-to-end against a kind cluster with two JSM instances pointing at different namespaces. Helm chart updated, runbook updated, CHANGELOG documented.
4. **Phase 3** lands a second integration **with no `internal/webhook/` package edits.** This is the abstraction's pressure test — if it fails, we revise the interfaces, not just the implementation.
5. The original JSM workflow (DESIGN-0002 / IMPL-0002) continues to work end-to-end without behavior changes visible to JSM-side callers. The synchronous response contract (ADR-0006) survives.

## Open Questions

> **Status:** Raised here for review during RFC sign-off; resolutions get baked into DESIGN-0004 before any code moves.

1. **Phase 3 ordering — AWS first or GitHub first?** AWS Backend exercises the "different backends from same provider" axis cleanly. GitHub stresses the "one vendor, two interfaces" axis. Both eventually land; the question is which one we want as the first abstraction-validator. Lightly leaning AWS (smaller scope; GitHub Provider has lots of event-shape variety to absorb).
2. **What goes in `Provider.IdempotencyKey` for providers that don't have a natural unique ID per event?** Slack does (`event_id`); GitHub does (`X-GitHub-Delivery`); JSM does (issue key). What about generic webhook providers? Likely return `""` (skip idempotency); document as a Provider-author choice.
3. **Should `webhook_id` be visible in the helm chart values, or generated at chart-render time?** Tradeoff: chart-render-time generation is GitOps-friendly but means the ID is checked into Git; explicit values keep them out of Git but require the operator to generate them. Probably explicit values + a `webhookd id generate` CLI subcommand, but worth thinking about.
4. **Hot-reload of the HCL config?** Defer to a follow-up RFC. v1 reads at startup only. Add when the operational pain shows up.

## References

### Decisions extracted from this RFC into standalone ADRs

These three decisions originated in this RFC's review but have been split into ADRs so future work can cite them independently. The ADRs own the full rationale; this RFC defers to them and keeps only the shape.

- **ADR-0008** — Provider-callback pattern over durable queues for long-running work. The headline architectural insight: webhookd uses each provider's own webhook-callback infrastructure (JSM ticket comments, GH check-runs, Slack `chat.postMessage`) instead of standing up NATS/Redis/Postgres for async dispatch.
- **ADR-0009** — HCL2 configuration format for multi-tenant instances. v1 wire format; supersedes ADR-0003 (env-only). Typed decoding via `gohcl.DecodeBody` collapses schema + decoding into one Go struct + tags artifact per integration. CRDs are a deferred upgrade for GitOps-per-instance.
- **ADR-0010** — Static integration registration via build-time imports. No Go plugins; no runtime loading; integrations register themselves via `init()` with an explicit opt-out path for tests.

### Source investigations

- **INV-0001** — *Multi-Provider Multi-Backend Architecture Review.* The investigation that produced this RFC. Source-of-truth for the resolved decisions on config format, pairing model, URL shape, idempotency, migration path, async strategy, and roadmap integrations. Read first.
- **INV-0002** — *Evaluate Argo Events as Alternative for JSM-to-CR Webhook Workflow.* The rejected-alternative investigation. Conclusion (Argo Events is async-by-design; cannot satisfy ADR-0006's synchronous response contract) carries forward to this RFC unchanged. Argo Events remains a candidate Backend integration for *future* fan-out workloads — that's a Phase 5+ direction, separate from this RFC.

### Designs and implementations carrying forward

- **DESIGN-0001** — Stateless webhook receiver. The substrate (HTTP, signing, observability, rate-limit) survives this refactor unchanged.
- **DESIGN-0002** — JSM webhook → SAMLGroupMapping CR provisioning. The current "JSM Provider + K8s Backend" reference pair. Status will flip to *Implemented-but-superseded* when DESIGN-0004 lands.
- **DESIGN-0003** — Helm chart and release pipeline. Phase 2 of this RFC requires chart updates to surface the HCL config (rendered into a `ConfigMap`); deployment story carries forward.
- **IMPL-0002 §Resolved Decisions** — current Provider/executor contract; the source-of-truth for what behavior must survive the refactor.

### ADRs that constrain or carry forward into this RFC

- **ADR-0001** — stdlib `net/http` routing. Multi-tenant routing builds on this, not against it.
- **ADR-0003** — Environment-variable-only configuration. *Superseded by ADR-0009 when this RFC lands.*
- **ADR-0004** — controller-runtime typed client. Constrains the K8s Backend implementation in Phase 0 and beyond.
- **ADR-0005** — Server-Side Apply. Constrains the K8s Backend implementation.
- **ADR-0006** — Synchronous response contract. The foundational choice this RFC reaffirms; the same constraint that disqualifies Argo Events in INV-0002 and underpins ADR-0008's callback-pattern design.
- **ADR-0007** — Trace context propagation via CR annotation. Continues to apply to the K8s Backend.

### Prior art

- Hookdeck, Tines, Argo Events, Knative Eventing — prior art for the Provider/Backend split.
