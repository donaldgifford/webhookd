---
id: INV-0003
title: "Pre-IMPL-0004 architectural review of webhookd"
status: Open
author: Donald Gifford
created: 2026-05-15
---
<!-- markdownlint-disable-file MD025 MD041 -->

# INV 0003: Pre-IMPL-0004 architectural review of webhookd

**Status:** Open
**Author:** Donald Gifford
**Date:** 2026-05-15

<!--toc:start-->
- [Question](#question)
- [Hypothesis](#hypothesis)
- [Context](#context)
- [Approach](#approach)
- [Environment](#environment)
- [Findings](#findings)
  - [Theme 1 — Multi-tenancy blockers (high severity)](#theme-1--multi-tenancy-blockers-high-severity)
  - [Theme 2 — Cross-package coupling](#theme-2--cross-package-coupling)
  - [Theme 3 — Error-handling architecture](#theme-3--error-handling-architecture)
  - [Theme 4 — Metrics architecture](#theme-4--metrics-architecture)
  - [Theme 5 — Lifecycle and concurrency](#theme-5--lifecycle-and-concurrency)
  - [Theme 6 — Performance and allocation](#theme-6--performance-and-allocation)
  - [Theme 7 — Documentation, naming, and test gaps](#theme-7--documentation-naming-and-test-gaps)
  - [Theme 8 — Style-guide nits (Uber Go Style)](#theme-8--style-guide-nits-uber-go-style)
- [Conclusion](#conclusion)
- [Recommendation](#recommendation)
- [References](#references)
<!--toc:end-->

## Question

What architectural debt exists in the webhookd runtime (`cmd/webhookd/`, `internal/`) that should be addressed *before* IMPL-0004 (multi-tenant Provider × Backend) begins, and how should that cleanup be sequenced relative to IMPL-0004 itself?

## Hypothesis

The single-tenant code written for IMPL-0001 and IMPL-0002 was correct at the time but has identifiable seams that will produce friction (or silent incorrectness) when extended for multi-tenancy. Expected outcome: a tractable list of cleanups, most low-cost, with a few high-value items concentrated around the **Executor's coupling to a single concrete CR type**, the **Metrics god-struct**, and the **Dispatcher's single-`ResponseBuilder`** assumption. Most other findings should be solvable in passing.

## Context

The webhookd codebase was written in two phases: IMPL-0001 (stateless HTTP receiver, HMAC verification, observability — PR #7) and IMPL-0002 (JSM provider → `SAMLGroupMapping` CR via K8s server-side-apply with synchronous watch-and-respond — PR #8). Both shipped before the team's Uber Go Style Guide skill and `go-architect` / `go-performance` agents existed.

RFC-0001 (`docs/rfc/0001-multi-tenant-provider-backend-architecture.md`) and DESIGN-0004 (`docs/design/0004-multi-tenant-provider-backend-foundation.md`) propose a multi-tenant Provider × Backend architecture but are forward-looking — the runtime code does **not** yet reflect them. IMPL-0004 has not started.

The team wants a clean architectural baseline before IMPL-0004 lands, so cleanup work can be sequenced deliberately rather than bundled into IMPL-0004 commits where it would obscure the multi-tenant work itself.

**Triggered by:** RFC-0001, DESIGN-0004, anticipated IMPL-0004 kickoff.

## Approach

1. Survey the codebase shape (~3,900 LoC across 12 packages, biggest files: `config.go` 463, `executor.go` 443, `main.go` 289, `metrics.go` 260, `middleware.go` 226, `jsm/provider.go` 208).
2. Run four specialist agents in parallel against the same source tree, each with a distinct lens:
   - **go-architect** — system design, package boundaries, abstractions, multi-tenancy readiness.
   - **go-performance** — allocations, hot-path patterns, label cardinality, span proliferation.
   - **go-style** — Uber Go Style Guide violations the linter doesn't catch.
   - **Explore** (very thorough) — independent free-form survey for dead code, doc gaps, test gaps, naming inconsistencies.
3. Deduplicate (~30% overlap between specialists, esp. on Executor coupling) and consolidate by theme.
4. Rank by severity: **high** = blocks IMPL-0004 cleanly or actively dangerous; **medium** = will become painful but tractable; **low** = nice-to-have.

## Environment

| Component             | Version / Value                                                              |
| --------------------- | ---------------------------------------------------------------------------- |
| Repo branch           | `docs/inv-arch-review` off `ba62882`                                          |
| Go (local / CI)       | 1.26.1 / 1.26.3                                                              |
| golangci-lint         | 2.11.4 (Uber-flavored)                                                       |
| controller-runtime    | v0.23.3                                                                      |
| k8s.io/*              | v0.35.0                                                                      |
| Scope (production)    | `cmd/webhookd/main.go` + `internal/` (~3,900 LoC, 27 production files)       |
| Out of scope          | `internal/webhook/wizapi/zz_generated.deepcopy.go` (auto-generated); tests   |

## Findings

Each finding has an ID (`F-NN`), location, severity, problem statement, and a one- or two-sentence suggested direction. Code is **not** proposed — direction only. Cross-references between agents are noted where the same issue surfaced from multiple lenses.

### Theme 1 — Multi-tenancy blockers (high severity)

These are the structural issues that will compound badly when adding a second Provider or Backend. **All should be addressed (or explicitly accepted) before IMPL-0004 begins.**

**F-01 — `Action` union references concrete `wizapi` type** (high)
- Location: `internal/webhook/action.go` (entire `ApplySAMLGroupMapping` struct).
- Problem: The `Action` union, intended as the dispatcher↔executor contract, embeds `wizapi.SAMLGroupMappingSpec` as a field. The "generic" execution layer is concrete to one CR shape from the start.
- Approach: Make `Action` a fully opaque interface; move `ApplySAMLGroupMapping` (and its `Spec` field) into either its own subpackage or into `internal/webhook/jsm`. The executor type-switches on the interface, not on a sibling-package concrete type.

**F-02 — Executor hardcoded to `SAMLGroupMapping`** (high)
- Location: `internal/webhook/executor.go:33` (`crKindLabel = "SAMLGroupMapping"`), `:244–278` (`waitForSync` constructs `&wizapi.SAMLGroupMappingList{}` and asserts `ev.Object.(*wizapi.SAMLGroupMapping)`), `:302–307` (`labels()` returns `LabelSource: "jsm"`).
- Problem: The executor is nominally generic — accepts any `Action` — but every Prometheus observation, every CR label, and the watch loop's type machinery are pinned to one provider × one CR kind. Adding a second backend means forking `Execute`/`waitForSync` or threading kind/source through every call.
- Approach: Extract a `SyncTarget` (or `Watchable`) interface that supplies (a) an empty list-typed object for `client.WithWatch.Watch()`, (b) a predicate that matches a single instance, and (c) a `Ready` checker. Carry `kind` and `source` on `ApplySAMLGroupMapping` (or pull from per-backend `ExecutorConfig`) and thread them into `observeApply`, `observeSync`, `labels()`.

**F-03 — `AnnotationIssue` is JSM-specific but lives on the executor** (high)
- Location: `internal/webhook/executor.go:44` (`AnnotationIssue = "webhookd.io/jsm-issue-key"`); stamped unconditionally by `annotations()` at `:315`.
- Problem: A JSM domain concept ("issue key") leaks into the generic K8s write path. Future backends without an "issue" concept will still stamp the annotation as `""`.
- Approach: Move provider-specific annotation keys onto the `Action` itself (e.g. an `Annotations map[string]string` field on `ApplySAMLGroupMapping`). The executor merges what it's given without knowing semantics. `AnnotationIssue` becomes a JSM-package constant.

**F-04 — `Dispatcher` holds a single `ResponseBuilder`, not a per-provider registry** (high)
- Location: `internal/webhook/dispatcher.go` (`DispatcherConfig.ResponseBuilder`, used unconditionally in `writeResponse`); wired in `cmd/webhookd/main.go` ~line 225.
- Problem: The seam exists (and a comment acknowledges "when a second provider lands, this becomes a per-provider lookup") but is not structurally enforced. The dispatcher will silently apply the JSM response shape to non-JSM providers until someone audits and fixes it.
- Approach: Either fold `ResponseBuilder` into the `Provider` interface (each provider builds its own response) or key builders by `p.Name()` in `NewDispatcher` alongside `d.providers`. The wiring already pairs them at construction time — formalize the co-location.

**F-05 — Hardcoded provider construction in `main.go`** (high)
- Location: `cmd/webhookd/main.go:195–231` (`buildDispatcher`).
- Problem: There is no provider registry. Adding a second provider requires editing `main.go` to construct it and pass it through. The HCL-based static registration pattern proposed in ADR-0010 isn't reflected anywhere yet.
- Approach: Introduce a registration interface — `webhook.RegisterProvider(name string, factory ProviderFactory)` — that integration packages call from `init()` (ADR-0010). `main.go` then iterates `cfg.EnabledProviders` and resolves each from the registry. This is the change ADR-0010 promised; it must land before a second provider.

**F-06 — `CRConfig` carries JSM/Wiz-specific `IdentityProviderID`** (medium → high under IMPL-0004)
- Location: `internal/config/config.go:117–143`.
- Problem: `CRConfig` is positioned as the shared CR-emitting-backend config, but `IdentityProviderID` is meaningful only to JSM/Wiz `SAMLGroupMapping`. A non-Wiz backend that emits CRs would still carry this field as dead config.
- Approach: Move `IdentityProviderID` into `JSMConfig`. Leave `CRConfig` containing only the structural K8s fields (`Namespace`, `FieldManager`, `SyncTimeout`).

### Theme 2 — Cross-package coupling

**F-07 — `internal/webhook/executor.go` imports `internal/httpx`** (medium)
- Location: `internal/webhook/executor.go:21`, called at `:317` via `httpx.RequestIDFromContext`.
- Problem: The K8s-side executor has a compile-time dependency on the HTTP middleware layer to read a request-scoped context value. The dependency arrow points the wrong way.
- Approach: Move `RequestIDFromContext` to a shared `internal/reqctx` (or similar context-key package), or define a context-key interface in `internal/webhook` that `httpx` implements. Either inverts the dependency without behavioral change.

**F-08 — `config.validate` contains JSM-specific branches** (medium)
- Location: `internal/config/config.go:309–322`.
- Problem: The central validator does `if cfg.ProviderEnabled("jsm") { check JSM-specific fields }`. Every new provider adds another `if cfg.ProviderEnabled("X")` block — the validator becomes the central registry of all provider-specific config rules.
- Approach: Introduce a `ProviderValidator` function type (or interface) registered alongside provider factories. `validate` iterates the enabled set and calls each registered validator; provider-specific rules live in provider packages.

**F-09 — `webhookd_jsm_response_total` recorded by generic dispatcher** (medium)
- Location: increment in `internal/webhook/dispatcher.go:169` (`writeResponse`); metric defined `internal/observability/metrics.go:66`.
- Problem: A metric named `webhookd_jsm_response_total` is incremented on every response, regardless of which provider produced it. When a second provider arrives, this metric will count non-JSM responses under a misleading name.
- Approach: Move the response counter increment into the per-provider `ResponseBuilder` (or a `RecordResponse(status int)` method on the Provider interface). Each provider drives its own response metric. Pair this with F-04.

**F-10 — JSM signature wrapper duplicates `webhook.Verify`** (low)
- Location: `internal/webhook/jsm/signature.go:50–62` wraps `internal/webhook/signature.go:100–112`.
- Problem: The wrapper exists only to inject JSM's configured header names. A second provider with the same v0 HMAC scheme will write an identical wrapper. The abstraction is inverted: the per-provider piece is *header resolution*, not signature verification.
- Approach: Move header-name plumbing into a small `HeaderNames` struct that providers pass to `webhook.Verify`. The wrapper goes away.

### Theme 3 — Error-handling architecture

**F-11 — `classifyK8sErr` default returns `ResultTransientFailure`** (medium)
- Location: `internal/webhook/executor.go:417–443`.
- Problem: Truly unknown errors (nil-pointer-derived, client-build failures, etc.) currently route to HTTP 503 + JSM retry. JSM will retry forever on a deterministic bug. The correct mapping for an unknown error is HTTP 500 + page a human.
- Approach: Change the `default` arm to `ResultInternalError`. Keep explicit cases for `IsServerTimeout`, `IsServiceUnavailable`, `IsTooManyRequests`, and `IsConflict` mapped to `ResultTransientFailure`. Anything not explicitly transient is internal.

**F-12 — Two parallel `classify*Err` helpers will become four** (low)
- Location: `internal/webhook/executor.go:413` (`classifyK8sErr`); `internal/webhook/dispatcher.go:183` (`classifyProviderErr`).
- Problem: Same shape (`error → ExecResult`), different domains. As backends and providers multiply, this pattern produces three, four, five non-shareable functions.
- Approach: Acknowledge the classification-registry pattern in a comment on `ExecResult` for now. The registry materializes naturally during IMPL-0004 — don't pre-build it.

### Theme 4 — Metrics architecture

**F-13 — `observability.Metrics` is a god-struct** (medium)
- Location: `internal/observability/metrics.go:21–70` — 14 fields; passed into `httpx`, `webhook`, `webhook/jsm`, `cmd/webhookd`.
- Problem: Every package that records *any* metric takes `*Metrics` and gets compile-time exposure to all 14. Adding a metric forces a rebuild of every consumer. Tests must stand up the full `NewMetrics` machinery to exercise one counter.
- Approach: Split into narrow consumer-side interfaces — `HTTPMetrics`, `K8sMetrics`, `JSMMetrics` (or `ProviderMetrics`) — each package accepting only the interface it uses. Concrete `Metrics` implements all of them; no behavioral change. Materially improves testability.

**F-14 — `JSMNoopTotal` label is unbounded user-controlled string** (HIGH — Prometheus cardinality risk)
- Location: emission `internal/webhook/jsm/provider.go:144`; metric def `internal/observability/metrics.go:238` — `webhookd_jsm_noop_total{trigger_status}`.
- Problem: Label value is `payload.Status()`, the raw JSM ticket status string. JSM tenants can rename or create arbitrary workflow states. A noisy or malicious tenant produces unbounded label cardinality → Prometheus OOM.
- Approach: Cap label values to a small allow-list (the configured `TriggerStatus` plus an `__other__` bucket), or hash/truncate strings above a safe length. **This is the only "unsafe today" finding in the review.**

**F-15 — Metrics construction split for funlen, not coherence** (low)
- Location: `internal/observability/metrics.go:102–201` (`NewMetrics`) plus `:207–244` (`addPhase2Metrics`).
- Problem: The split exists to keep `NewMetrics` under the linter's line budget. Anyone reading the function sees Phase-1 metrics; Phase-2 metrics are appended by a private helper. A new metric added in the wrong half could leave a struct field nil.
- Approach: Group metrics by domain (`buildHTTPMetrics`, `buildK8sMetrics`, `buildJSMMetrics`) and have `NewMetrics` compose them. The split is then *semantic* rather than line-count-driven.

**F-16 — `e.metrics == nil` nil-safety is undocumented contract** (low)
- Location: `internal/webhook/executor.go:377–390`; `internal/webhook/jsm/provider.go:138–145`.
- Problem: Both packages check `if metrics == nil { return }` to permit nil-metrics tests, but the contract is implicit. Callers don't know whether nil is OK without reading the implementation.
- Approach: Document the nil-acceptable contract on `ExecutorConfig.Metrics` and `jsm.Config.Metrics`. Alternative: require non-nil and provide a `metrics.Noop()` constructor.

**F-44 — Early-out HTTP error responses bypass the response counter** (medium)
- Location: `internal/webhook/dispatcher.go:124, 132, 137, 142` (four early-return paths via `http.Error`); counter increment lives at `:169` inside `writeResponse`.
- Problem: Responses for `404 unknown provider`, `401 invalid signature`, `413 body too large`, and `400 read body failed` never increment `webhookd_jsm_response_total`. Operators reading the counter assume it represents *all* responses; it represents only the post-verification subset. SLOs built off this metric will understate true error rates by exactly the noisiest-but-most-interesting categories.
- Approach: Hoist the counter increment into a deferred `statusRecorder.Status()` call at the top of `ServeHTTP` (synergy with F-22, which already discusses status-recorder reuse). Pair with F-09's rename so the metric is provider-agnostic before the wider counter is enabled.

### Theme 5 — Lifecycle and concurrency

**F-17 — `k8s.Scheme` is a package-level mutable `var`** (low)
- Location: `internal/k8s/scheme.go:30, 32–39`.
- Problem: Mutated at `init()` via `utilruntime.Must(scheme.AddToScheme(...))`. Tests that want an isolated scheme have to construct their own; nothing prevents accidental concurrent mutation later.
- Approach: Expose `NewScheme() *runtime.Scheme`; have `NewClients` accept an optional `*runtime.Scheme` (functional option). The default behavior — package-level shared scheme — stays. Tests gain an isolation path.

**F-18 — `startServer` goroutine wait pattern relies on channel buffer matching goroutine count** (medium — Uber style "error")
- Location: `cmd/webhookd/main.go:243–251`.
- Problem: Two goroutines send into a buffered channel of capacity 2; `waitForShutdown` reads at most one. The pattern is correct *as long as the buffer matches goroutine count* — a fragile invariant. Adding a third listener silently breaks shutdown ordering.
- Approach: Use a `sync.WaitGroup` to explicitly wait for both goroutines after `drainServers` returns, or drain all pending sends from the channel post-shutdown. Either makes the contract explicit.

**F-19 — `httpx.NewServer` takes the full `*config.Config`** (low)
- Location: `internal/httpx/server.go:28`.
- Problem: Violates the narrow-config-struct pattern documented in CLAUDE.md and followed everywhere else. Uses only four timeout fields.
- Approach: Add a `ServerConfig` struct mirroring `AdminConfig` / `RateLimitConfig`. Map at the call site in `main.go`.

### Theme 6 — Performance and allocation

These are mostly low/medium; flag-worthy primarily because they signal architectural smells.

**F-20 — Double JSON unmarshal in `jsm.Decode`** (medium)
- Location: `internal/webhook/jsm/payload.go:123–136`.
- Problem: Every payload unmarshals into `map[string]json.RawMessage` for an `"issue"` probe, then again into `*Payload`. Two passes, two sets of allocations per request.
- Approach: Probe via a custom top-level `UnmarshalJSON` on `Payload` that checks for the `"issue"` key inline (the same trick `IssueFields` already uses for custom-field detection).

**F-21 — `json.NewEncoder` allocated per response** (medium)
- Location: `internal/webhook/dispatcher.go:173`.
- Problem: Fresh `*json.Encoder` + 512-byte internal buffer per response. Compounds under multi-tenant load.
- Approach: Use `json.Marshal` + `w.Write(b)`, or a `sync.Pool` of `*bytes.Buffer` reused via `json.NewEncoder(buf)`.

**F-22 — `SLog` and `Metrics` middlewares each allocate their own `statusRecorder`** (medium)
- Location: `internal/httpx/middleware.go:136, 165`.
- Problem: Two heap allocations per request for the same wrapper concern; two layers of virtual dispatch on every `Write`. Smells like ad-hoc coupling between observably-similar middlewares.
- Approach: Merge `SLog` and `Metrics` into a single combined observability middleware, or hoist the status recorder into a shared context value populated once.

**F-23 — `labels()` and `annotations()` allocate maps unnecessarily** (low)
- Location: `internal/webhook/executor.go:302–307` (`labels`), `:312–324` (`annotations`).
- Problem: `labels()` returns a freshly-allocated 2-entry map of constants on every apply. `annotations()` allocates without `make(_, 4)` pre-sizing.
- Approach: Promote `crLabels` to a package-level `map[string]string` constant (cloned only if the caller intends to mutate). Pre-size `annotations` with `make(map[string]string, 4)`.

**F-24 — `crName` uses `strings.ToLower` then rune-iterates** (low)
- Location: `internal/webhook/executor.go:342–360`.
- Problem: Allocates the lowercased intermediate, then ranges over runes. Input is guaranteed ASCII (`[A-Z]+-[0-9]+`).
- Approach: Iterate bytes; combine lowercase (`b | 0x20`) and the character-class switch into one pass. Saves the intermediate string allocation. *Note: This is trivia — one webhook = one call.*

**F-25 — `writeBody([]byte(body))` on every probe** (low)
- Location: `internal/httpx/admin.go:88`.
- Problem: `/healthz` and `/readyz` allocate a `[]byte` from a constant string on every request. Kubelet hits these at probe frequency.
- Approach: Use `io.WriteString(w, body)`.

**F-42 — `strconv.Itoa(status)` allocated per response over a bounded status set** (low)
- Location: `internal/webhook/dispatcher.go:169`.
- Problem: HTTP response status codes for webhookd are a small bounded set (200, 400, 401, 413, 422, 500, 503, 504). `strconv.Itoa` allocates a new string on every response purely to feed it as a Prometheus label.
- Approach: Pre-populate a `var statusLabels = map[int]string{...}` and look up. Saves the allocation; the lookup is faster than `strconv.Itoa` for tiny ints anyway.

### Theme 7 — Documentation, naming, and test gaps

**F-26 — Exported label/annotation constants in executor have no doc comments** (medium)
- Location: `internal/webhook/executor.go:39–45` (`LabelManagedBy`, `LabelSource`, `AnnotationTraceID`, `AnnotationReqID`, `AnnotationIssue`, `AnnotationApplied`).
- Problem: Some of these are part of the writer-side contract that ADR-0007 documents (`AnnotationTraceID`). Without doc comments, callers can't tell which are public contract vs. internal label.
- Approach: Add one-line doc comments. Group public-contract annotations into a separate `const (...)` block from internal labels.

**F-27 — `Action`'s sealed-variant pattern is undocumented** (low)
- Location: `internal/webhook/action.go` (the `isAction()` sentinel method).
- Problem: Callers see an interface they can't implement; the package doc doesn't explain why. New contributors will read this as a bug.
- Approach: Add a package doc comment on `Action` explaining the sealed variant pattern and listing the canonical implementations.

**F-28 — Three production files have no direct tests** (medium)
- Location: `internal/webhook/action.go`, `internal/webhook/provider.go`, `internal/webhook/providertest/mock.go`.
- Problem: Provider/Action interface contracts are exercised only transitively via dispatcher and executor tests. Breaking-change additions to either interface won't fail compilation outside `providertest`.
- Approach: Add a small `provider_test.go` with compile-time `var _ Provider = ...` checks for `jsm.Provider` and `providertest.Mock`. (`providertest.Mock` already has one inline; pull it into a test.) Add basic round-trip tests for `Action` variants and the `ResultKind` enum.

**F-29 — Set-but-disabled config is silently ignored** (low)
- Location: `internal/config/config.go:309–320`.
- Problem: An operator setting `WEBHOOK_JSM_FIELD_ROLE` but forgetting `WEBHOOK_PROVIDERS=jsm` gets a clean startup. The misconfiguration only surfaces when they try to fire a webhook.
- Approach: After validation, if any provider-specific env var is set but the provider is not enabled, log a warning. Don't fail — operators legitimately stage config ahead of enablement — but make the silent ignore visible.

**F-30 — Critical helpers unexported in `internal/webhook`** (low)
- Location: `internal/webhook/executor.go` — `crName` (`:342`), `syncOutcome` (`:364`), `applyOutcome` (`:399`), `classifyK8sErr` (`:417`).
- Problem: `crName` in particular is DNS-1123 normalization of an external identifier — algorithmically load-bearing. Operators debugging CR naming will only find it in source.
- Approach: Move the per-CR-kind name builder into a `CRNamer` function on the `Action` interface (paired with F-02). The classifier helpers stay unexported.

**F-41 — `NewDispatcher` duplicate-provider-panic is untested** (low)
- Location: `internal/webhook/dispatcher.go:107–113`.
- Problem: The panic-on-duplicate invariant is a deliberate architectural choice ("crash loudly at startup") but no test exercises it. A future refactor that flips to silent overwrite or `errors.New(...)` would pass CI silently.
- Approach: Add one test that calls `NewDispatcher` with two providers whose `Name()` collides; assert the panic via `defer recover()`. Five-line test, ten-year invariant.

**F-43 — `traceIDFromContext` defined in `executor.go` but used by `dispatcher.go`** (low)
- Location: defined `internal/webhook/executor.go:330`, called from `internal/webhook/dispatcher.go:161`.
- Problem: Cross-file helper inside the same package — correct technically, but its location signals "executor-private" to readers. Future contributors may duplicate it elsewhere rather than discover it.
- Approach: Move to a small `internal/webhook/context.go` or merge with the request-ID helper as part of F-07's fix. Makes the shared-helper status explicit.

### Theme 8 — Style-guide nits (Uber Go Style)

A handful of straightforward fixes the linter doesn't enforce. None blocking; bundle into a single "style sweep" PR before IMPL-0004 kickoff.

| ID    | Location                                                          | Violation                                                                                       | Rule                  |
| ----- | ----------------------------------------------------------------- | ----------------------------------------------------------------------------------------------- | --------------------- |
| F-31  | `internal/httpx/middleware.go:40`                                  | Unexported global `requestIDKey` missing `_` prefix                                              | uber:global-name      |
| F-32  | `internal/observability/metrics.go:75,83,90`                       | Three histogram-bucket globals missing `_` prefix                                                | uber:global-name      |
| F-33  | `internal/httpx/ratelimit.go:22`                                   | `webhookPathPrefix` missing `_` prefix                                                           | uber:global-name      |
| F-34  | `internal/webhook/executor.go:28,33`                               | `tracerName`, `crKindLabel` missing `_` prefix                                                   | uber:global-name      |
| F-35  | `internal/webhook/signature.go:41,46`                              | `signaturePrefix`, `canonicalVersion` missing `_` prefix                                         | uber:global-name      |
| F-36  | `internal/webhook/wizapi/types.go:22–28, 83–89, 112–118` (×5)      | Missing blank line between embedded `metav1` fields and regular fields                            | uber:struct-embed     |
| F-37  | `internal/webhook/jsm/payload.go:96, 123`                          | `map[string]json.RawMessage{}` instead of `make(...)`                                            | uber:map-init         |
| F-38  | `internal/webhook/executor.go:177, 235, 244`                       | `&wizapi.SAMLGroupMapping{}` instead of `var x wizapi.SAMLGroupMapping; &x`                       | uber:struct-zero      |
| F-39  | `internal/webhook/dispatcher.go:38`                                 | `Dispatcher` missing `var _ http.Handler = (*Dispatcher)(nil)` interface-compliance check        | uber:interface-compliance |
| F-40  | `internal/webhook/executor.go:28, 33, 39–45`                       | Standalone `const`s adjacent to a `const (...)` block — should be grouped                         | uber:decl-group       |

## Conclusion

**Answer: Confirmed.** There is meaningful pre-IMPL-0004 cleanup, but the scope is bounded — 34 narrative findings + 10 style nits (44 total), of which 6 are high-severity (all in Theme 1), 11 are medium, and the rest are low. No issue is catastrophic; one (**F-14, JSM noop label cardinality**) is unsafe today and should be hot-fixed regardless of IMPL-0004 sequencing. A post-publication rescan of `action.go` / `provider.go` / `dispatcher.go` surfaced four additional findings (F-41–F-44), one of which (**F-44, error responses bypass response counter**) is a medium-severity observability gap.

The hypothesis held: the highest-value cleanups concentrate around the **Executor's coupling to a single concrete CR type** (F-01, F-02, F-03), the **Dispatcher's single-`ResponseBuilder` assumption** (F-04), **hardcoded provider construction in `main.go`** (F-05), and the **Metrics god-struct** (F-13). Most other findings are tractable in passing — but the Theme-1 items will compound nonlinearly once a second provider exists.

The four-agent parallel review pattern was effective: ~30% overlap between specialists (especially on executor coupling — surfaced by both `go-architect` and `Explore`) confirmed those findings; the remaining 70% were genuinely lens-specific (cardinality risk from `go-performance`; style rules from `go-style`; doc gaps and test-coverage gaps from `Explore`). Recommend the same multi-agent pattern for future architectural reviews.

## Recommendation

**Sequencing:** three tracks, in order.

1. **Hot-fix (separate PR, before anything else).**
   - **F-14** — bound `webhookd_jsm_noop_total{trigger_status}` cardinality. Allow-list or `__other__` bucket. Single small commit, ship before IMPL-0004 begins (or sooner). This is the only finding marked *unsafe today*.

2. **Pre-IMPL-0004 cleanup PRs (open a `plan` doc to scope).**
   - Theme 1 (F-01 through F-06) **must** land before IMPL-0004 starts — they define the seams IMPL-0004 will fill in. Suggest one PR per finding, each small enough to review in 15 minutes.
   - Theme 2 (F-07 through F-10) **should** land before IMPL-0004 — bundle into one "coupling cleanup" PR if convenient.
   - F-11 (classifyK8sErr default) — small standalone behavior fix; should ship pre-IMPL-0004 so the new error-shape baseline is the multi-tenant reference.
   - F-13 (Metrics god-struct) — narrow consumer interfaces are inexpensive and unlock cleaner IMPL-0004 testing; recommend pre-IMPL-0004.
   - F-44 (response counter bypasses error paths) — pair with F-09's rename so the metric is provider-agnostic and complete at the same time; pre-IMPL-0004 so the baseline metric is trustworthy.

3. **IMPL-0004-adjacent cleanup (fold into IMPL-0004 phases).**
   - F-12 (classify-helper pattern) — defer; the registry materializes inside IMPL-0004 naturally.
   - F-15, F-16, F-17 (metrics construction split, nil-safety doc, scheme package var) — fold into the IMPL-0004 phase that touches the same code.
   - F-18, F-19 (lifecycle / config-shape nits) — IMPL-0004 phase 0 or 1.
   - F-20, F-21, F-22, F-23, F-24, F-25, F-42 (perf/allocation) — defer unless we see real load problems; revisit during IMPL-0004 performance pass.
   - F-26 through F-30, F-41, F-43 (doc / test / organization gaps) — bundle into a single "doc-and-test sweep" PR before IMPL-0004 kickoff.
   - F-31 through F-40 (Uber style nits) — one batch PR; uncontroversial.

**Next step.** Open PLAN-XXXX to lay out the cleanup work as discrete PRs (Themes 1, 2, 3 and the doc-sweep + style-sweep). The plan doc references this investigation; each PR references the relevant finding ID. If the team decides not to pursue some findings (e.g., F-22 is judged not worth the refactor), record the deferral inline in the plan doc with a one-line rationale.

**Out of scope for this investigation.** Performance benchmarking, security audit, dependency-policy review. F-14 is a Prometheus cardinality concern — not a security finding, though it shares the "untrusted-input" smell with one.

## References

- **Triggering specs:** RFC-0001 (`docs/rfc/0001-multi-tenant-provider-backend-architecture.md`), DESIGN-0004 (`docs/design/0004-multi-tenant-provider-backend-foundation.md`), ADR-0008 / 0009 / 0010 (`docs/adr/0008-…md`, `docs/adr/0009-…md`, `docs/adr/0010-…md`).
- **Implementation context:** IMPL-0001 (`docs/impl/0001-…md`), IMPL-0002 (`docs/impl/0002-…md` — esp. Resolved Decisions §2 on CRD shape).
- **Style references:** Uber Go Style Guide (via `go-development:go` skill).
- **Related investigations:** INV-0001 (`Multi-Provider Multi-Backend Architecture Review` — established the design space this review is preparing for), INV-0002 (`Evaluate Argo Events as Alternative for JSM-to-CR Webhook Workflow` — the build-vs-buy spike that informed staying in-tree).
- **Review agents (this investigation):** `go-development:go-architect`, `go-development:go-performance`, `go-development:go-style`, `Explore`.
