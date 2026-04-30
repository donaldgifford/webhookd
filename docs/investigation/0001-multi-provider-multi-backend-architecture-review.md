---
id: INV-0001
title: "Multi-Provider Multi-Backend Architecture Review"
status: Open
author: Donald Gifford
created: 2026-04-30
---

<!-- markdownlint-disable-file MD025 MD041 -->

# INV 0001: Multi-Provider Multi-Backend Architecture Review

**Status:** Open **Author:** Donald Gifford **Date:** 2026-04-30

<!--toc:start-->
- [Question](#question)
- [Hypothesis](#hypothesis)
- [Context](#context)
- [Approach](#approach)
- [Findings](#findings)
  - [Current architecture: what we actually have](#current-architecture-what-we-actually-have)
  - [The Provider/Backend split: clean idea, one terminology snag](#the-providerbackend-split-clean-idea-one-terminology-snag)
  - [Routing model: /{provider}/{webhook_id} is the right shape](#routing-model-providerwebhookid-is-the-right-shape)
  - [Config: HCL2 vs YAML vs CRDs vs env](#config-hcl2-vs-yaml-vs-crds-vs-env)
  - [State management: do we need it?](#state-management-do-we-need-it)
    - [Long-running work via provider callbacks (not queues)](#long-running-work-via-provider-callbacks-not-queues)
    - [Per-provider idempotency keys (Q4 expanded)](#per-provider-idempotency-keys-q4-expanded)
  - [Plugin avoidance: the right call](#plugin-avoidance-the-right-call)
  - [Composition: pipeline vs eventbus vs pass-through](#composition-pipeline-vs-eventbus-vs-pass-through)
- [Conclusion](#conclusion)
- [Recommendation](#recommendation)
- [Resolved Decisions](#resolved-decisions)
- [References](#references)
<!--toc:end-->

## Question

Should webhookd be refactored from its current "JSM-shaped, K8s-shaped"
structure into a generalized **Provider × Backend** service that can host
arbitrary input integrations (jsm, gh, slack, …) and dispatch them to arbitrary
output systems (k8s, aws, http, …) — with multi-tenant routing
(`/{provider}/{webhook_id}`), HCL2 config, and (eventually) a story for
horizontal scale?

## Hypothesis

The user's instinct is right that the current shape is too narrow for the stated
end-goal. The cheapest path forward is **rename + decouple**, not a ground-up
rewrite: today's `Action` type union is already a well-formed boundary between
input parsing and output execution; renaming it to `BackendRequest` (or similar)
and replacing the K8s-only `Executor` with a `Backend` interface gives us 80% of
the proposed architecture for ~15% of the diff.

State management (Redis/NATS/Postgres) is **not needed** for the workloads we
have. Backends that exceed sync HTTP-timeout budgets use the **provider-callback
pattern** — POST the result back to the originating system's callback URL —
which reuses each provider's existing webhook-retry infrastructure instead of
inventing a parallel durable-queue path. Durable queues only enter scope if a
real failure mode shows up that callbacks can't address.

YAML is the right config format for v1; HCL2 (better hierarchical config) and
CRDs (better GitOps) are clean upgrades to revisit later. The wire format is
independent of the in-memory struct, so the swap is contained.

## Context

**Triggered by:** Conversation 2026-04-30 — user request to review the codebase
and consider whether the current shape generalizes to a multi-tenant webhook
service.

The repo today ships a single Provider (`jsm`) wired to a single Backend
(Kubernetes Server-Side Apply of a `SAMLGroupMapping` CR). DESIGN-0001 +
IMPL-0001 built the substrate (HTTP, signing, observability, rate-limit).
DESIGN-0002 + IMPL-0002 added JSM-specific parsing and the K8s executor.
DESIGN-0003 + IMPL-0003 packaged it for deployment. The Helm chart and release
pipeline are now in place — this is the first investigation that asks whether
the substrate underneath is the right shape going forward.

The user's stated end-goal: receive webhooks from many vendors (jsm, gh, slack,
…) and dispatch them to many downstream systems (k8s, aws, wiz, http, …),
routing via `/{provider_type}/{webhook_id}` so the same provider type can host
multiple tenant-specific webhook instances. Single binary; no Go plugins.

## Approach

1. Survey the current `internal/webhook/` package and surrounding wiring
   (`internal/config`, `cmd/webhookd/main.go`) to identify which pieces are
   already general and which are JSM/K8s-shaped.
2. Map the user's proposed Provider/Backend model onto the current code —
   identify what would move, get renamed, or get deleted.
3. Compare the proposal against three alternative shapes: pipeline
   (Provider→Filter→Transform→Backend), eventbus (Provider→queue→Backend), and
   pass-through (Provider→Backend, no intermediate state).
4. Evaluate state-management options (none / pod-local / Redis / NATS /
   Postgres-outbox) against the actual stateful needs webhookd has today and
   would acquire under each backend mode (sync vs async).
5. Evaluate config formats (env / YAML / HCL2 / CRDs) against the multi-tenant
   routing requirement.
6. Recommend a phased path forward.

## Findings

### Current architecture: what we actually have

Looking at the live code — not the docs:

| Layer            | File                                    | What it does                                                                                                     | Generalizable today?                                                                                 |
| ---------------- | --------------------------------------- | ---------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------- |
| HTTP entry       | `internal/webhook/dispatcher.go`        | Routes `POST /webhook/{provider}` → Provider lookup → body limit → signature → Handle → Execute → response       | ✅ Mostly. Already keyed by `{provider}` path value.                                                 |
| Provider seam    | `internal/webhook/provider.go`          | `Name() / VerifySignature() / Handle(body) → (Action, error)`                                                    | ✅ Clean. Pure-function `Handle` is exactly the right shape.                                         |
| Action union     | `internal/webhook/action.go`            | Closed-set sentinel-method union: `NoopAction`, `ApplySAMLGroupMapping`. New variants must land in this package. | ⚠️ Closed-set is the leak: every new backend means editing this file _and_ the executor's switch.    |
| Execution        | `internal/webhook/executor.go`          | K8s-only. Hardcodes `crKindLabel = "SAMLGroupMapping"`, takes `client.WithWatch`, owns the SSA + Watch loop.     | ❌ Names + dependencies are K8s-shaped. This is what would become `Backend`.                         |
| Response shaping | `dispatcher.go` (`ResponseBuilder`)     | Provider-specific body shape (JSM cares about `crName`, `traceId`, `requestId` for ticket comments)              | ⚠️ Single field on Dispatcher; should be per-provider lookup.                                        |
| Config           | `internal/config/config.go` (463 lines) | All env-var. Pre-baked `JSMConfig` and `CRConfig` structs. `WEBHOOK_PROVIDERS=jsm` allow-list.                   | ❌ Single-tenant by design. No way to express "two JSM webhooks pointing at different K8s clusters." |
| Routing          | `cmd/webhookd/main.go:174`              | `mux.Handle("POST /webhook/{provider}", dispatcher)` — single path variable                                      | ❌ One instance per provider type.                                                                   |

**The good news:** the Provider interface is _already_ the right shape.
`Handle(ctx, body) → (Action, error)` is pure, side-effect-free, and the
dispatcher already separates "decide what to do" from "do it". The split the
user is describing is _almost_ there — it's just been hardcoded onto K8s/JSM.

**The gap:** there is no `Backend` interface. The `Action` type union is the
wrong shape for this — it forces a closed enumeration in the `webhook` package,
which means a new backend (e.g., post to AWS SNS) requires editing this central
file plus the executor's switch arm. That's the single biggest blocker to
generalization.

### The Provider/Backend split: clean idea, one terminology snag

The user's framing is correct and matches every webhook-broker product on the
market (Hookdeck, Tines, n8n, Argo Events, Knative Eventing, Pipedream): **input
integrations are different beasts from output integrations.**

One pushback on the wording — the user said:

> the Jsm interface would implement the provider and backend interface

In practice this is **almost never** true. JSM is purely an _input_ source:
webhookd receives notifications from it. The _output_ is JSM's own automation
rule reading our HTTP response body — and that's a side-effect of the HTTP
transport, not a "backend." Vendors that genuinely double as both (e.g., GitHub:
receive `pull_request.opened`, post a PR comment) end up shipping **two distinct
concrete types** in the same package — `github.Provider` and `github.Backend` —
that share configuration but implement disjoint interfaces. They don't fuse into
one type.

So the model I'd propose is:

```go
// Input side. One implementation per vendor that sends webhooks.
type Provider interface {
    Name() string
    VerifySignature(r *http.Request, body []byte) error
    Handle(ctx context.Context, body []byte) (BackendRequest, error)
}

// Output side. One implementation per downstream system.
// BackendRequest is whatever the provider produced — opaque to the dispatcher,
// the matching backend knows how to consume it.
type Backend interface {
    Name() string
    Execute(ctx context.Context, req BackendRequest) ExecResult
}

// BackendRequest replaces the closed Action union. Open-ended via interface,
// but each provider-backend pair agrees on a concrete type at wiring time.
type BackendRequest interface {
    BackendName() string  // routing key: which backend handles this
}
```

Two design knobs to pick:

**Knob 1: how does a `BackendRequest` get to the right `Backend`?**

- (a) **Static wiring at config-time:** every webhook instance binds one
  provider to one backend. The `BackendRequest` carries no routing info; the
  dispatcher knows from config.
  - Pro: simplest, easiest to reason about. Most webhook brokers in production
    work this way.
  - Con: rigid — fan-out (one event → multiple backends) needs separate webhook
    instances.
- (b) **Type-driven dispatch:** `BackendRequest.BackendName()` returns a string,
  dispatcher looks up the backend by name.
  - Pro: a Provider can produce different request types and have them routed to
    different backends from the same incoming webhook.
  - Con: one more thing to get wrong. Most use cases don't need it.

→ Recommendation: start with (a). It covers every actual use case I can think of
and (b) is a non-breaking upgrade later.

**Knob 2: do Providers know about Backends, or stay decoupled?**

The current Provider returns `Action` — a typed value with K8s shape leaking
through (`ApplySAMLGroupMapping.Spec wizapi.SAMLGroupMappingSpec`). The cleanest
refactor keeps Providers ignorant of backends: a Provider returns a _request
object_ that's opaque from the dispatcher's perspective; the matching Backend
knows how to consume it.

This means **`internal/integrations/jsm-to-k8s/`** (or wherever) ships:

- A `jsm.Provider` that produces a `K8sApplyRequest` (when paired with a K8s
  backend) or an `HTTPPostRequest` (when paired with an HTTP backend).
- The pairing happens at config time, not in code.

The closed `Action` union becomes an open seam: each (Provider, Backend) pair
agrees on a concrete request type, the dispatcher just plumbs bytes-of-the-thing
from one to the other.

### Routing model: `/{provider}/{webhook_id}` is the right shape

Today: `POST /webhook/{provider}` → exactly one instance per type.

Proposed: `POST /webhook/{provider}/{id}` → N instances of each type, each
instance has its own:

- Signing secret
- Backend binding (which downstream system + its config)
- Provider-specific config (JSM custom field IDs, GH event filters, …)

This is **standard** for multi-tenant webhook services. Stripe uses an opaque
path token; Hookdeck uses `/{source_id}`; GitHub Actions doesn't bother (it's
per-repo). The pattern is well-trodden.

Two implementation notes:

1. **`{id}` should be opaque to the URL.** Don't put tenant names or anything
   sensitive in the path — it lands in access logs, Prometheus labels (if you're
   not careful), and request URL spans. Use a short random ID (e.g., 12 base32
   chars) generated at config time. The README's existing JSM doc already kind
   of does this — JSM tenants don't see the URL.
2. **Dispatcher lookup becomes a two-key map**: `(provider, id) → instance`. The
   instance carries the provider config + backend binding. Routing cost is
   constant.

### Config: HCL2 vs YAML vs CRDs vs env

The user asked for HCL2. Worth steel-manning the alternatives.

**Option A — Stay env-only.** ❌ Doesn't work. Multi-tenant config doesn't fit
cleanly into env. You'd end up with `WEBHOOK_INSTANCE_0_PROVIDER=jsm`,
`WEBHOOK_INSTANCE_0_ID=abc123`, … which is just YAML pretending to be env vars.

**Option B — YAML config file.** Simple. Helm chart already produces YAML; users
already know it. No extra deps. Ergonomics: fine for static config, awkward for
anything dynamic.

```yaml
instances:
  - id: abc123
    provider:
      type: jsm
      triggerStatus: Approved
      fields:
        {
          providerGroupID: customfield_10001,
          role: customfield_10002,
          project: customfield_10003,
        }
      signing: { secretEnv: WEBHOOK_JSM_TENANT_A_SECRET }
    backend:
      type: k8s
      namespace: wiz-operator
      identityProviderID: tenant-a-idp
```

**Option C — HCL2 config file.** More expressive: variables, conditionals,
validators, function calls, dynamic blocks. Used by Terraform, Packer, Vault.

```hcl
instance "abc123" {
  provider "jsm" {
    trigger_status = "Approved"
    fields = {
      provider_group_id = "customfield_10001"
      role              = "customfield_10002"
      project           = "customfield_10003"
    }
    signing { secret_env = "WEBHOOK_JSM_TENANT_A_SECRET" }
  }
  backend "k8s" {
    namespace            = "wiz-operator"
    identity_provider_id = "tenant-a-idp"
  }
}
```

**Pros (HCL2):**

- Genuinely better at hierarchical, repeated structures (which webhookd has).
- Strong validators built into the language (`validation { condition = ... }`).
- Block-style reads like a config DSL, not data-as-config.
- Dynamic blocks let you generate N instances from a list.

**Cons (HCL2):**

- One more dep (`github.com/hashicorp/hcl/v2`) — but it's stable,
  well-maintained, BSD-3.
- Less common in cloud-native (most operators use YAML/CRDs); contributors will
  have a small learning curve.
- Tooling (e.g., `helm template`-style rendering) is less ubiquitous than YAML.

**Option D — CRDs (Kubernetes-native).** A `Webhook` CRD where each instance is
a Kubernetes object. webhookd watches and reloads.

- Pro: declarative, GitOps-native, RBAC-able per-instance, integrates with
  operators we already deploy.
- Pro: free admission validation via the schema.
- Con: ties webhookd to running on Kubernetes. Right now the binary is
  K8s-agnostic — only the K8s _backend_ depends on K8s. CRDs make K8s a
  substrate dependency.
- Con: hot-reload story is operator-shaped; we'd be re-writing
  controller-runtime for our own CRD.

→ My recommendation: **YAML (Option B) for v1 of the refactor.** It's a small,
reversible decision; if HCL2 ergonomics turn out to matter, swap it out later.
The wire format is independent of the in-memory `Config` struct, so the swap is
contained.

If you genuinely want HCL2, the cost is small and I won't fight you on it — but
I want to flag that it's a "we'll appreciate it later" investment, not a "we
need it now" requirement.

### State management: do we need it?

This is the most interesting question. Let's enumerate **what state webhookd
actually has or might have**, and pin which storage each needs:

| State                                    | What                                                                                                                                | Where today                                                | Where at scale                                                                                                                         |
| ---------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------- |
| Replay protection (security)             | HMAC timestamp window — reject if `\|now - ts\| > skew`                                                                             | Stateless (just clock comparison)                          | Stateless. Same as today.                                                                                                              |
| Nonce de-duplication (security)          | "Same signature within window? Drop."                                                                                               | ❌ Not implemented today (we rely on timestamp skew alone) | If added: Redis SET with TTL, or pod-local LRU if 1-pod is acceptable. Distinct from idempotency keys (below).                         |
| Per-provider idempotency (correctness)   | "Same logical event already in-flight? Drop the duplicate." Provider-specific key (JSM ticket key, GH delivery-id, Slack event-id). | ❌ Not implemented today                                   | Pod-local `sync.Map` keyed by `(provider, idempotencyKey)`, populated on entry, cleared on completion. See dedicated subsection below. |
| In-flight backend work (current JSM→K8s) | Watch on CR until Ready=True                                                                                                        | Pod-local goroutine                                        | Pod-local. Caller is sync — pod restart = caller retries.                                                                              |
| Long-running async work                  | Provider POSTs back to a callback URL when work completes                                                                           | ❌ Not applicable (sync)                                   | **No durable queue needed** if backends use the callback pattern. See "Long-running work via provider callbacks" below.                |
| Audit log                                | "What came in, what went out, when, with what trace ID?"                                                                            | Logs (slog → stdout)                                       | Logs (continue), or structured event store if compliance requires it.                                                                  |
| Multi-pod coordination                   | Leader election for cron-style backends                                                                                             | ❌ Not applicable                                          | Required only if a backend type does scheduled work.                                                                                   |

#### Long-running work via provider callbacks (not queues)

User pushback on the original framing of this doc: "the easy fix for async is
the provider implemented to that backend needs a return webhook to hit if fail
or success — JSM supports it." That's correct, and it changes the recommendation
materially.

The pattern works like this:

1. JSM POSTs to webhookd with the request payload + a `callbackURL` field (or
   webhookd derives it from the JSM API).
2. webhookd verifies signature, parses, and decides:
   - If the backend's work fits inside the HTTP timeout (current case: K8s SSA +
     watch for Ready, ~seconds): respond synchronously, like today.
   - If the work won't fit (hypothetical AWS-Lambda-followed-by-Step-Function
     backend, ~minutes): return `202 Accepted` immediately with a correlation
     token, then continue the work in a goroutine. When the goroutine completes
     (success/failure/timeout), POST the result to JSM's callback endpoint with
     HMAC-signed body.
3. JSM's automation rule reads the callback POST and updates the ticket
   accordingly.

**Why this beats a durable queue:**

- **Zero new infrastructure.** No NATS, no Redis, no Postgres outbox. The
  "queue" is the originator's own retry-on-failure semantics: if webhookd's pod
  dies mid-work, the inbound webhook gets re-delivered (most providers support
  this), the new pod picks it up via the original signed payload, idempotency
  keys (above) prevent duplicate work, and the callback fires from whoever
  finishes.
- **Provider-native.** JSM, GitHub, Slack, Linear, PagerDuty, etc. all support
  inbound webhooks for status updates. Reusing that path means we don't invent a
  new contract.
- **Operationally honest.** Instead of "we have a durable queue, now operations
  needs to monitor + back up a NATS cluster," we're just doing one HTTP POST
  out. Failure modes are observable in metrics + logs, not buried in queue lag
  dashboards.
- **Pod restart is recoverable.** The goroutine is lost; the inbound webhook
  re-delivery handles it. The idempotency key prevents the second attempt from
  doing duplicate work _if_ the first attempt completed before crashing.

**Where it doesn't work:**

- The originating provider doesn't support inbound webhook callbacks. (Rare for
  SaaS; common for some internal/legacy systems. Use a queue if you need to
  integrate with one of those.)
- The work needs **fan-out** to N backends with independent retry semantics
  (e.g., one event → "create CR + post Slack + create GitHub issue, retry each
  independently"). A durable queue or Argo Events shines here. See INV-0002 —
  this is exactly Argo Events' sweet spot.
- Compliance requires a durable audit trail of every event (logs aren't enough).
  Then you want a Postgres outbox that doubles as the audit store.
- **Pod crash between "work done" and "callback POSTed."** This is the genuine
  sharp edge of the callback pattern. Three ways out:
  1. **Trust callback idempotency at the provider level (cheap, default).** For
     most providers, sending the same status update twice produces the same
     outcome — duplicate JSM comments are annoying but not broken; GH check-runs
     API is idempotent on `external_id`; Slack `chat.postMessage` accepts a
     `client_msg_id` for dedup. v1 of the callback shape leans on this.
  2. **Per-CR callback-fired annotation (medium).** When the callback POST
     returns 2xx, patch the target CR with
     `webhookd.io/callback-fired-at: <RFC3339>`. On retry, peek at the
     annotation before re-firing. Note: this is _separate_ from the existing
     `webhookd.io/jsm-issue-key` annotation — that one is a correlation marker
     stamped at apply time, _not_ a dedupe marker for the callback step. Costs
     one extra K8s patch per webhook; survives pod restart cleanly. Add when
     option (1) hits a provider that's unhappy with double-fires.
  3. **Durable queue + state (Phase 5, deferred).** The principled fix when
     failure modes accumulate to the point where annotation-on-CR is no longer
     carrying the load.

**Refined recommendation for state management:**

1. **Phase 1 of the refactor: stay synchronous, callback-pattern ready.** Every
   backend implements `Execute(ctx, req) → Result` synchronously _or_
   `Execute(ctx, req) → (Pending, callbackFunc)` for long-running work. The
   dispatcher's contract supports both shapes; today's K8s backend stays sync,
   and we have a clear path to async without new infra.
2. **No NATS, no Redis, no Postgres outbox in the refactor scope.** Defer those
   _only_ if a future backend hits one of the "where it doesn't work" cases
   above.
3. **If durable infrastructure ever does become necessary, NATS JetStream is the
   right choice** — it was designed for this shape. But reaching for it should
   require a written justification ("provider X doesn't support callbacks" or
   "we need fan-out with independent retries").

#### Per-provider idempotency keys (Q4 expanded)

This is a different concern from HMAC nonce dedup, and worth calling out
separately because the user's question about "we already have an inflight
request for ticket SEC-1234" pointed at it.

| Mechanism                   | What it protects against                                                                | Key                                                              | Storage                       |
| --------------------------- | --------------------------------------------------------------------------------------- | ---------------------------------------------------------------- | ----------------------------- |
| Timestamp skew              | Replay of an old signed request after long delay                                        | Implicit (timestamp value)                                       | Stateless                     |
| Nonce dedup (optional)      | Replay of a signed request _within_ the skew window                                     | HMAC signature digest                                            | Pod-local LRU or Redis        |
| Idempotency keys (proposed) | Duplicate processing of the same logical event (e.g., JSM retries because we were slow) | Provider-derived: JSM ticket key, GH delivery-id, Slack event-id | Pod-local `sync.Map` with TTL |

The idempotency key is **provider-specific**: each Provider implementation
contributes a function `IdempotencyKey(payload) string` that returns the natural
ID for that vendor's payload. The dispatcher wraps every request in:

```go
key := provider.IdempotencyKey(payload)
if !inflight.Acquire(provider.Name(), key) {
    return 200 // already in flight, dedup
}
defer inflight.Release(provider.Name(), key)
// ... normal processing
```

Pod-local is fine for the common case (a single retrying caller hits the same
pod within seconds via L7 load-balancer affinity, or worst case the duplicate
hits a different pod and gets processed twice — same outcome as today).
Cross-pod idempotency would need shared storage; we don't pay that cost until a
real failure mode shows up.

### Plugin avoidance: the right call

Go's `plugin` package is a footgun:

- Every plugin must be built with the _exact same_ Go version, _exact same_
  dependency versions, and _exact same_ GOOS/GOARCH as the host.
- Plugins can't be unloaded.
- Cross-plugin types are second-class — the host has to traffic in `interface{}`
  and assert.
- The whole module graph gets duplicated in memory.

Static registration via build-time imports is the right answer. The pattern:

```go
// cmd/webhookd/main.go imports every integration package; each registers
// itself via init() into a package-level registry. Adding an integration
// = one import line + a new directory.
import (
    _ "github.com/donaldgifford/webhookd/internal/integrations/jsm"
    _ "github.com/donaldgifford/webhookd/internal/integrations/github"
    _ "github.com/donaldgifford/webhookd/internal/integrations/k8s"
    _ "github.com/donaldgifford/webhookd/internal/integrations/http"
)
```

(`init()`-based registration has its own critics — the alternative is an
explicit `RegisterAll()` function called from main. Either is fine; `init()` is
more idiomatic for this kind of registry.)

The cost is a slightly bigger binary as integrations accumulate. At 10
integrations × ~500 LOC each, you're talking +50 KB compiled. Not a real cost.

### Composition: pipeline vs eventbus vs pass-through

The user described pass-through (Provider → Backend, direct). Worth naming the
alternatives so we're explicit about _not_ picking them:

- **Pass-through (current, proposed):** Provider → Backend, synchronous. The
  dispatcher is a thin wire. ✅ Simple, low-latency, easy contracts.
- **Pipeline:** Provider → Filter → Transform → Backend. Each stage is
  composable. Like n8n, Zapier, Argo Events. ❌ Massive complexity bump for use
  cases we don't have. The pure-Provider `Handle(body) → BackendRequest` already
  lets you do filter+transform inside the provider; pulling it out into stages
  is YAGNI.
- **Eventbus:** Provider → publishes event → Backend subscribes. ❌ Adds queue
  infrastructure for the privilege of decoupling. Useful when fan-out matters;
  we can grow into it if/when async backends land.

→ Stick with pass-through. It's what's already there and it's correct for the
workload.

## Conclusion

**Answer:** Yes, the refactor is the right move — but the cost is much lower
than initially assumed, because the current Provider interface is already 80% of
the proposed shape. The work is mostly:

1. Rename `Action` → `BackendRequest` and open the union (interface, not closed
   sentinel-method type).
2. Extract a `Backend` interface from `Executor`. Move the K8s-specific code
   into `internal/integrations/k8s/` as one Backend implementation.
3. Move JSM out of `internal/webhook/jsm/` into `internal/integrations/jsm/` as
   one Provider implementation.
4. Replace single-tenant env config with multi-tenant YAML config: a list of
   `Instance{ID, Provider, Backend, ProviderConfig, BackendConfig}`.
5. Change routing from `/webhook/{provider}` to `/webhook/{provider}/{id}`,
   two-key dispatcher lookup.
6. Static, build-time integration registration via `init()` or explicit
   `RegisterAll()`. No Go plugins.
7. Add per-provider idempotency (pod-local `sync.Map`) — different concern from
   HMAC nonce dedup, both can be added.
8. Backends that exceed sync HTTP-timeout budgets use the **provider-callback
   pattern** (POST result back to the originating system), not a durable queue.

**No NATS, no Redis, no Postgres outbox in scope.** The callback pattern handles
long-running work without new infrastructure; durable queues only enter the
picture if a real failure mode shows up that callbacks can't address (provider
doesn't support inbound webhooks, fan-out with independent retries, compliance
audit trail).

**Hard cutover at the next minor version** — nothing's live yet, no migration
shim needed.

## Recommendation

**Follow up with two documents (in this order):**

1. **RFC** — "Generalize webhookd to Provider × Backend with multi-tenant
   routing." Settles the high-level shape (interfaces, registration, routing,
   config format, idempotency, callback pattern) before any code moves. Open the
   existing IMPL-0002/IMPL-0003 status as Implemented-but-superseded; this RFC
   is the next major arc.
2. **DESIGN** — concrete interface signatures, file layout, config schema,
   routing details. Becomes "DESIGN-0004: Multi-tenant Provider/Backend
   architecture." No migration plan needed (cutover, not migration).

**Refactor sequencing:**

- Phase 0: Move existing code into the new package layout _without changing
  behavior_. `internal/webhook/jsm/` → `internal/integrations/jsm/`.
  `internal/webhook/executor.go` → `internal/integrations/k8s/backend.go`. All
  tests still pass. (Pure rename; reviewable in one sitting.)
- Phase 1: Introduce `Backend` interface; the K8s implementation now satisfies
  it. `Action` becomes `BackendRequest`. Add per-provider idempotency keys.
  Still single-tenant per provider.
- Phase 2: Multi-tenant — YAML config file, `(provider, id)` routing, instance
  lookup map. **Hard cutover** from JSM env vars to config-driven instances at
  the same release.
- Phase 3: Add a second integration to validate the abstraction. Roadmap
  candidates: `aws` Backend (fits the "second backend, different provider
  pairing" test) or `github` Provider+Backend pair (validates the "two
  interfaces in one package" pattern explicitly).
- Phase 4: Add the callback-pattern shape to `Backend` (`Execute` returns either
  `Result` or `(Pending, callback)`). Touch only when a backend actually needs
  it.
- Phase 5 (genuinely deferred, possibly never): durable queue / shared state.
  Only if the callback pattern hits a wall.

## Resolved Decisions

Each numbered decision below corresponds to an open question raised during
initial review of this INV. Recorded with reasoning so the rationale survives.

1. **Config format: YAML for now, HCL2 or CRDs later.** YAML covers the v1
   multi-tenant shape with no new deps. HCL2 (better for hierarchical/repeated
   config) and CRDs (better for GitOps + per-instance RBAC) are both viable
   upgrades, but neither pulls weight today. The wire format is independent of
   the in-memory `Config` struct, so the swap is contained when it happens.

2. **Provider/Backend pairing: static.** Each webhook instance binds one
   provider to one backend at config time; the `BackendRequest` carries no
   routing info, the dispatcher knows from config. Confirmed by the user's
   framing — the backend is _not_ tied to the provider type (JSM provider goes
   to K8s today, AWS later), and that pairing is a config-time choice. Routed
   dispatch (`BackendRequest.BackendName()`) is a non-breaking upgrade if it's
   ever needed.

3. **URL path shape: `/{provider_type}/{webhook_id}` with opaque random IDs.**
   Splitting the routing key is genuinely better than flat `/{webhook_id}` for
   four reasons:
   - **Routing layer separation:** ingress/load-balancer rules can split by path
     prefix (e.g., apply different rate-limit pools per provider type at the
     edge).
   - **Operator ergonomics:** `kubectl logs ... | grep "/jsm/"` filters to one
     provider's traffic without parsing log fields.
   - **Metrics labels:** `provider` is a stable, low-cardinality dimension we'd
     want as a label anyway; pulling it from the URL keeps that consistent.
   - **Fail-fast on misconfig:** wrong provider in URL → 404 at routing time,
     not signature-verify failure later. Earlier failure = clearer error.

   IDs are opaque random strings (e.g., 12 base32 chars) generated at config
   time. No tenant names, no human-readable slugs — those leak into access logs,
   span attributes, and metrics labels.

4. **Replay protection: timestamp skew + per-provider idempotency keys.** Two
   distinct mechanisms (see expanded subsection in Findings):
   - **Timestamp skew (existing):** keeps. Stateless, protects against old
     replayed signed requests.
   - **Per-provider idempotency keys (new):** each Provider contributes a pure
     `IdempotencyKey(payload) string` — JSM uses ticket key, GH uses
     delivery-id, Slack uses event-id. Dispatcher gates with a pod-local
     `sync.Map[(provider, key)]`; duplicates respond 200-noop without
     re-processing. Pod-local is fine; cross-pod coordination only enters the
     picture if a real failure mode shows up.

   Nonce-based HMAC dedup (drop a signature digest after first sight) remains
   optional and orthogonal — neither replaces the other.

5. **Migration: hard cutover.** Nothing's live yet. No env-compat shim, no
   synthesized single-instance config, no `--legacy-env` flag. New config format
   goes live at the same release that introduces multi-tenant routing.

6. **Long-running work: provider-callback pattern, not durable queues.** The
   original framing in this doc treated "async" as a deployment-architecture
   question requiring NATS/Redis. The user's reframing — "if the work won't fit
   in the HTTP timeout, the provider has a callback URL the backend hits when
   done" — is **the right answer for the workloads we have.** It uses each
   provider's existing webhook-callback infrastructure (JSM ticket comments /
   status transitions, GH check-runs API, Slack chat.postMessage, etc.) instead
   of inventing a parallel durable-queue path.

   For the current K8s-SSA-and-watch backend: the work fits in the HTTP timeout,
   no callback needed. Stay sync, like today.

   For a hypothetical long-running backend (e.g., AWS Step Function that takes
   10 minutes): respond `202 Accepted` with a correlation token, run the work in
   a goroutine, POST the final result to the originator's callback URL when
   done. Pod-restart recovery is provided by the originator's webhook-retry
   behavior + idempotency keys (decision §4).

   **The pod-crash-between-work-and-callback edge case is acknowledged.** v1
   leans on provider-side callback idempotency (the cheap, default option). If a
   specific provider integration shows it's unhappy with double-fires, the
   medium-cost fix is a `webhookd.io/callback-fired-at` annotation on the target
   CR — _separate from_ the existing `webhookd.io/jsm-issue-key` annotation,
   which is a correlation marker stamped at apply time, not a callback-step
   dedupe marker. The principled fix beyond that is the Phase 5 durable-queue
   path. See the "Long-running work via provider callbacks" subsection in
   Findings for the full enumeration.

   Durable queues (NATS JetStream, Postgres outbox) are **only** justified by
   failure modes the callback pattern + annotation-on-CR can't cover: provider
   doesn't support inbound webhooks, fan-out to N backends with independent
   retries, compliance-driven event audit trail, or accumulated callback-dedupe
   complexity outgrowing what annotations can carry. None apply today; revisit
   when one does.

7. **Roadmap integrations: jsm-to-k8s today; aws backend, github (Provider AND
   Backend) next.** Confirmed:
   - **JSM Provider + K8s Backend** is the existing pair, becomes the reference.
   - **AWS Backend** validates the "providers and backends are not 1:1 — JSM can
     target K8s or AWS" model. Likely a synchronous backend (calls AWS APIs,
     returns the response).
   - **GitHub Provider + GitHub Backend** is the explicit pressure test for "one
     vendor ships both interfaces in one package as two distinct types"
     (§Provider/Backend split, terminology snag). Receive `pull_request.opened`,
     post a PR comment — different types, shared client + auth config.

   Each phase adds one of these and can blow up an unstated assumption in the
   abstraction; staging them gives us cheap revision points before the API is
   locked.

## References

- DESIGN-0001 — Stateless webhook receiver (Phase 1 substrate that survives this
  refactor unchanged)
- DESIGN-0002 — JSM → SAMLGroupMapping provisioning (the current "JSM Provider +
  K8s Backend" pair)
- DESIGN-0003 — Helm chart and release pipeline (deployment story; needs minor
  updates to surface multi-instance config)
- IMPL-0002 §Resolved Decisions — current Provider interface contract
- ADR-0004 — controller-runtime typed client (constrains the K8s Backend
  implementation)
- ADR-0005 — Server-Side Apply (constrains the K8s Backend implementation)
- ADR-0006 — synchronous response contract (the foundational choice this
  investigation reaffirms)
- INV-0002 — Argo Events as alternative for the JSM workflow (companion
  investigation; reaches the same conclusion that durable-queue infrastructure
  is overkill for our shape)
- Hookdeck, Tines, Argo Events, Knative Eventing — prior art for the
  Provider/Backend split
- Webhook-callback / async-with-callback patterns (JSM ticket comments, GitHub
  Checks API, Slack chat.postMessage) — the mechanism we'll reach for instead of
  durable queues for any future long-running backend
- NATS JetStream, Postgres outbox pattern — reference points kept on file for
  the (deferred, possibly never) case where the callback pattern hits a wall
