---
id: ADR-0010
title: "Static integration registration via build-time imports"
status: Proposed
author: Donald Gifford
created: 2026-04-30
---
<!-- markdownlint-disable-file MD025 MD041 -->

# 0010. Static integration registration via build-time imports

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

Proposed (extracted from RFC-0001).

## Context

RFC-0001 generalizes webhookd to host arbitrary Provider implementations (jsm, github, slack, …) and Backend implementations (k8s, aws, http, …). Each integration lives in its own package under `internal/integrations/<name>/`. The dispatcher needs a way to find the registered integrations at runtime so that, given a YAML config saying `provider.type: jsm` and `backend.type: k8s`, it can resolve those strings to concrete Go types.

Three load mechanisms were considered:

- **Go's `plugin` package** — `.so` files loaded at runtime, separately compiled. The "plugin" word the user explicitly ruled out, but worth recording why.
- **Static, build-time imports** — every integration is a Go package compiled into the binary; each registers itself with a central registry via either `init()` or an explicit `RegisterAll()` call from `main`.
- **External process per integration** — sidecar containers, gRPC, etc. Out of scope; we'd be writing a different system.

The constraint set:

- One binary, one pod per webhookd instance. No multiple processes, no IPC.
- Adding an integration should be cheap (one new package + one import line), not require rebuilding a whole new toolchain or shipping multiple artifacts.
- Test isolation must be possible — tests shouldn't have to load every integration just to exercise one.
- Versions of the host and integrations are always consistent (same Go version, same dep graph).
- Predictable startup; no late-binding surprises, no missing-plugin runtime errors.

## Decision

webhookd uses **static, build-time imports** for integration registration. Each integration package is compiled into the binary; registration happens via `init()` with an opt-out path for tests.

```go
// cmd/webhookd/main.go — every integration is a build-time import.
import (
    _ "github.com/donaldgifford/webhookd/internal/integrations/jsm"
    _ "github.com/donaldgifford/webhookd/internal/integrations/github"
    _ "github.com/donaldgifford/webhookd/internal/integrations/k8s"
    _ "github.com/donaldgifford/webhookd/internal/integrations/aws"
)
```

Each package's `init()` registers itself:

```go
// internal/integrations/jsm/init.go
func init() {
    webhook.RegisterProvider(&Provider{})
}

// internal/integrations/github/init.go — vendor that ships both
func init() {
    webhook.RegisterProvider(&Provider{})  // github.Provider — input
    webhook.RegisterBackend(&Backend{})    // github.Backend  — output
}
```

The central registry (`internal/webhook/registry.go`) exposes `RegisterProvider`, `RegisterBackend`, and `LookupProvider(type) Provider`, `LookupBackend(type) Backend`. Duplicate registration of the same `Type()` panics at startup — preferable to silently routing to whichever package was imported last.

For tests, the registry exposes `webhook.NewRegistry()` returning an isolated registry that callers can populate explicitly. Tests that exercise one integration construct an empty registry and register only what they need; tests that need the full production registry import the integration packages explicitly. `init()` is a convenience for the production binary, not a hard requirement of the registration API.

Go's `plugin` package is explicitly rejected. External-process integrations are out of scope for v1.

## Consequences

### Positive

- **Simple, predictable startup.** Every integration is present at compile time. No "plugin not found" runtime errors, no version-skew failure modes.
- **Single artifact.** One binary, one image, one Helm chart deployment unit. Same blast radius as today.
- **Type-safe registration.** Providers and Backends are concrete Go types with full method-set checking; no `interface{}` traffic, no reflection, no type assertions in the registry.
- **Cheap to add an integration.** One new package + one import line + (typically) one short `init()` function.
- **Single dependency graph.** All integrations resolve their deps at the same `go.mod` boundary; no per-plugin module duplication, no diamond-dep nightmares.
- **Test isolation is achievable.** `webhook.NewRegistry()` lets tests opt out of the global registration path entirely.

### Negative

- **Binary grows linearly with integration count.** At ~500 LOC per integration with stdlib-only dependencies, 10 integrations is ~+50 KB compiled — negligible. Heavier integrations (e.g., AWS SDK pulls in tens of MB) inflate this materially.
- **Adding an integration requires rebuilding webhookd.** Operators can't drop in a new `.so` without redeploying. For most teams this is a feature, not a bug — release discipline stays inside the existing CI pipeline. For teams that need plugin-style hot-loading, it's a limitation.
- **`init()` ordering is package-import-order, not deterministic across all configurations.** If two integrations register on the same `Type()`, whichever runs first wins or panics. We panic at first registration of a duplicate; mitigates the silent-failure mode.
- **Forks that want to add private integrations need to rebuild from source.** Proprietary integrations live in a fork or a private replace directive. Standard pattern; not unique to this design.

### Neutral

- The registry is initialized at package-init time, before `main` runs. Anything that reads the registry must do so after `init()` has completed — i.e., during or after `main`. This is the normal Go program lifecycle.
- Integrations could move from `internal/integrations/` to a separate top-level repo (`webhookd-integrations/`) if the count grows enough to warrant it. Doing so doesn't change this ADR — `init()`-based registration works the same way across module boundaries.

## Alternatives Considered

- **Go's `plugin` package** (`.so` files, `plugin.Open`). Rejected.
  - Plugins must be compiled with the *exact same* Go version, *exact same* module graph (including `go.sum` hashes), and *exact same* GOOS/GOARCH as the host. Any drift produces a runtime load failure.
  - Plugins can't be unloaded; reloads require process restarts.
  - Plugins traffic in `interface{}` and `plugin.Symbol`, requiring type assertions at the boundary — eroding the type-safety win we get from a typed `Provider` / `Backend` interface.
  - Whole module graph is duplicated in memory between host and plugin.
  - The marginal benefit (drop-in integrations without rebuilding) doesn't apply to our deployment model — we're a release-disciplined service running on Kubernetes with proper CI.
- **External-process integrations** (sidecar container per integration; gRPC over UDS or localhost TCP). Rejected as out-of-scope and over-engineered for the workloads at hand.
  - Pros: language-agnostic; integration crashes don't take down webhookd; clean process isolation.
  - Cons: multiple processes per pod, IPC overhead per webhook, separate auth/transport/observability stack per integration, deployment becomes substantially more complex. If we ever needed integrations written in Python or Rust, this becomes worth revisiting; we don't.
- **Explicit `RegisterAll()` from `main`** instead of `init()`. The cleaner-but-more-verbose alternative — `main` calls `jsm.Register(reg)`, `github.Register(reg)`, etc. Pros: no surprising package-import side effects, easier to control test setup. Cons: one extra line per integration, mainly stylistic. Decision: use `init()` as the default for production wiring, but expose `webhook.NewRegistry()` so tests can opt out and use the explicit pattern. Both styles are supported.
- **Reflection-based discovery.** Walk a directory of registered packages, instantiate via reflection. Rejected as Go-anti-pattern; build-time imports are the idiomatic answer.

## References

- RFC-0001 — Generalize webhookd to Provider × Backend with Multi-Tenant Routing (the parent proposal that surfaces this decision).
- INV-0001 §Plugin avoidance: the right call (the original reasoning).
- [`plugin` package documentation](https://pkg.go.dev/plugin) — the rejected option, kept on file for the next person who proposes it.
- Idiomatic Go registry pattern: `database/sql` driver registration via `init()` is the canonical reference.
