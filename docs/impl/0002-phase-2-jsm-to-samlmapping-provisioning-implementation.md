---
id: IMPL-0002
title: "Phase 2 JSM to SAMLGroupMapping Provisioning Implementation"
status: Draft
author: Donald Gifford
created: 2026-04-27
---
<!-- markdownlint-disable-file MD025 MD041 -->

# IMPL 0002: Phase 2 JSM to SAMLGroupMapping Provisioning Implementation

**Status:** Draft
**Author:** Donald Gifford
**Date:** 2026-04-27

<!--toc:start-->
- [Objective](#objective)
- [Scope](#scope)
  - [In Scope](#in-scope)
  - [Out of Scope](#out-of-scope)
- [Implementation Phases](#implementation-phases)
  - [Phase 0: Bootstrap & Dependencies](#phase-0-bootstrap--dependencies)
    - [Tasks](#tasks)
    - [Success Criteria](#success-criteria)
  - [Phase 1: Config Additions](#phase-1-config-additions)
    - [Tasks](#tasks-1)
    - [Success Criteria](#success-criteria-1)
  - [Phase 2: Provider Interface & Action Union](#phase-2-provider-interface--action-union)
    - [Tasks](#tasks-2)
    - [Success Criteria](#success-criteria-2)
  - [Phase 3: K8s Client & Scheme](#phase-3-k8s-client--scheme)
    - [Tasks](#tasks-3)
    - [Success Criteria](#success-criteria-3)
  - [Phase 4: Executor (K8s Apply + Sync Watch)](#phase-4-executor-k8s-apply--sync-watch)
    - [Tasks](#tasks-4)
    - [Success Criteria](#success-criteria-4)
  - [Phase 5: JSM Provider](#phase-5-jsm-provider)
    - [Tasks](#tasks-5)
    - [Success Criteria](#success-criteria-5)
  - [Phase 6: Dispatcher & Application Wiring](#phase-6-dispatcher--application-wiring)
    - [Tasks](#tasks-6)
    - [Success Criteria](#success-criteria-6)
  - [Phase 7: Observability Additions](#phase-7-observability-additions)
    - [Tasks](#tasks-7)
    - [Success Criteria](#success-criteria-7)
  - [Phase 8: RBAC, Sample Manifests, Documentation](#phase-8-rbac-sample-manifests-documentation)
    - [Tasks](#tasks-8)
    - [Success Criteria](#success-criteria-8)
- [File Changes](#file-changes)
- [Testing Plan](#testing-plan)
- [Dependencies](#dependencies)
- [Resolved Decisions](#resolved-decisions)
  - [Cross-doc follow-ups](#cross-doc-follow-ups)
- [References](#references)
<!--toc:end-->

## Objective

Land the Phase 2 provisioning pipeline described in DESIGN-0002: turn the
Phase 1 receiver from a "verify-and-log" service into an actionable pipeline
that translates a JSM status-transition webhook into a `SAMLGroupMapping` custom
resource, applies the CR via Server-Side Apply, watches its status until the
Wiz operator marks it `Ready=True` (or surfaces a terminal failure), and
returns a synchronous response to JSM that maps cleanly onto its retry
semantics.

The emphasis of Phase 2 is **the pipeline shape**, not multi-provider
extensibility. We introduce the minimum interface boundary needed to keep the
JSM logic self-contained (`Provider`, `Action`, `Dispatcher`, `Executor`) so
that adding the second concrete provider in Phase 3 is mechanical, but we do
not build a plugin framework. Every line in this phase is in the path of a
real ticket; nothing is speculative.

**Implements:** DESIGN-0002 (JSM Webhook → SAMLGroupMapping Provisioning — Phase 2).

**Builds on:** IMPL-0001 (Phase 1 stateless receiver). The middleware chain,
admin mux, observability spine, rate limiter, and graceful-shutdown logic are
unchanged. Only the `/webhook/{provider}` handler is replaced.

## Scope

### In Scope

- Provider interface (`webhook.Provider`) and Action union (`webhook.Action`,
  `webhook.NoopAction`, `webhook.ApplySAMLGroupMapping`).
- Dispatcher that routes `/webhook/{provider}` by path-value to a registered
  provider and forwards the returned action to the executor.
- Executor that applies the CR via SSA and synchronously waits for sync via
  `watch.UntilWithSync`.
- JSM provider package (`internal/webhook/jsm`) implementing `Provider`:
  payload decode, custom-field extraction (single `providerGroupId` /
  `role` / `project` strings; cardinality 1:1), spec build, signature
  verification, response shaping.
- Kubernetes client wiring: controller-runtime typed `client.Client`,
  scheme registration with the operator's `wizapi` types
  (`SAMLGroupMapping`, `Project`, `UserRole`), in-cluster +
  `KUBECONFIG` config sources.
- Config additions (`WEBHOOK_JSM_*`, `WEBHOOK_CR_*`, `WEBHOOK_KUBECONFIG`)
  with validation, defaults, and tests.
- Observability additions: new metrics on the `Metrics` struct, new spans
  around decode/extract/apply/watch/respond, trace-id annotation stamped onto
  the CR.
- Migration of Phase 1's `/webhook/{provider}` handler: the existing
  `webhook.NewHandler(...)` constructor and its tests are replaced by the
  dispatcher. Phase 1 signature helpers (`Verify`, `VerifyHMAC`,
  `VerifyTimestamp`, the v0: canonical) are kept as a public helper package
  callable from any provider that wants Slack-style HMAC.
- Integration tests using `envtest` (in-process K8s API server) for the apply
  + watch path, including the operator-simulator (status patcher) used by
  the happy / failure / timeout cases.
- Unit tests + a `FuzzJSMDecode` fuzz target for the JSM payload decoder.
- RBAC sample manifest (`deploy/rbac/`) granting webhookd's ServiceAccount
  the verbs DESIGN-0002 §RBAC requires.

### Out of Scope

- Multi-provider plugin system, capability declarations, per-provider config
  schema framework. Phase 2 ships exactly one provider; the interface is the
  organizing seam.
- Worker-queue / async execution path (DESIGN-0002 explicitly defers).
- CR delete / cleanup path on ticket cancellation or re-open.
- Drift reconciliation outside the operator's loop.
- Multi-namespace fan-out (one CR namespace, configurable).
- Operator-side tracing changes (annotation read + remote-parent linking).
  Owned by the operator team.
- Helm chart and full deployment manifests. Sample RBAC ships with this
  phase; chart work continues to live in its own follow-up.
- End-to-end (kind + real operator + Wiz sandbox) test automation. The
  envtest suite is the merge gate; the end-to-end test stays a manual
  pre-release check.

## Implementation Phases

Each phase builds on the previous. A phase is complete when all its tasks
are checked off and its success criteria are met. Phases are sized for
individual commits or small PRs, mirroring the IMPL-0001 cadence.

---

### Phase 0: Bootstrap & Dependencies

Pin the new module dependencies, register the operator's API types in the
project, and establish the envtest harness so later phases can run
integration tests locally and in CI without further plumbing.

#### Tasks

- [x] Add direct module imports to `go.mod`:
  - [x] `sigs.k8s.io/controller-runtime` (v0.23.3 pinned; transitive
        from `k8s.io/apimachinery` v0.36.0).
  - [x] `k8s.io/apimachinery` (direct), `k8s.io/client-go`
        (transitive via controller-runtime).
  - [x] `sigs.k8s.io/controller-runtime/pkg/envtest` (test-only;
        ships inside controller-runtime).
- [x] Run `go mod tidy`; verify the toolchain still resolves under
      `mise install`.
- [x] Add `make tools-envtest` target that fetches `setup-envtest` and
      installs the matching Kubernetes binaries (`kube-apiserver`,
      `etcd`, `kubectl`) into `build/envtest/k8s/<version>/`. Wire
      `KUBEBUILDER_ASSETS` in `make test` so envtest finds them.
- [x] Confirm `make ci` still passes with no business-logic changes
      (just module additions + `make tools-envtest` available locally).
- [x] Land the local types stub at `internal/webhook/wizapi/`:
      `GroupVersion = schema.GroupVersion{Group: "wiz.webhookd.io",
      Version: "v1alpha1"}`, plus `SAMLGroupMapping{Spec, Status}`,
      `Project{Spec, Status}`, `UserRole{Spec, Status}` types matching
      the YAML shapes in `docs/examples/samples/`, plus `AddToScheme`.
      Hand-written DeepCopy methods in `zz_generated.deepcopy.go` so
      `client.Client` accepts them. Replaced by a one-line re-export
      from `github.com/donaldgifford/wiz-operator/api/v1alpha1` once
      that module is published.

#### Success Criteria

- `go build ./...` succeeds with the new dependencies.
- `make ci` is green (lint, test, build, license-check).
- `make tools-envtest` materializes the K8s test binaries and a smoke test
  (`go test -run=TestEnvtestStub ./internal/webhook/...`) can start +
  shut down an envtest control plane locally.
- Local `internal/webhook/wizapi` stub is in place and registered in
  a single scheme builder so Phases 3+ have one import to depend on.
  (Swapped to the published operator module in a later commit.)

---

### Phase 1: Config Additions

`internal/config` — extend `Config` and `Load()` with the JSM and CR
variables from DESIGN-0002 §Config Additions. No business logic outside
parsing / validation.

#### Tasks

- [x] Add a nested `JSMConfig` struct on `Config`:
  - [x] `TriggerStatus string` (`WEBHOOK_JSM_TRIGGER_STATUS`, default
        `Ready to Provision`).
  - [x] `FieldProviderGroupID string` (`WEBHOOK_JSM_FIELD_PROVIDER_GROUP_ID`,
        **required when provider enabled**) — JSM custom-field ID for the
        SSO group name (becomes `spec.providerGroupId`).
  - [x] `FieldRole string` (`WEBHOOK_JSM_FIELD_ROLE`, **required when
        provider enabled**) — JSM custom-field ID for the role name
        (becomes `spec.roleRef.name`, references a `UserRole` CR).
  - [x] `FieldProject string` (`WEBHOOK_JSM_FIELD_PROJECT`, **required
        when provider enabled**) — JSM custom-field ID for the project
        name (becomes `spec.projectRefs[0].name`, references a `Project`
        CR).
- [x] Add a nested `CRConfig` struct on `Config`:
  - [x] `Namespace string` (`WEBHOOK_CR_NAMESPACE`, default `wiz-operator`).
  - [x] `APIGroup string` (`WEBHOOK_CR_API_GROUP`, default
        `wiz.webhookd.io`). Used for log/metric labels and a startup
        sanity-check against `wizapi.GroupVersion.Group` (fail-fast if
        config and imported types disagree). The typed client uses the
        imported `wizapi.GroupVersion` for the actual GVK on the wire.
  - [x] `APIVersion string` (`WEBHOOK_CR_API_VERSION`, default `v1alpha1`).
  - [x] `FieldManager string` (`WEBHOOK_CR_FIELD_MANAGER`, default
        `webhookd`).
  - [x] `SyncTimeout time.Duration` (`WEBHOOK_CR_SYNC_TIMEOUT`, default
        `20s`). Validate `> 0` and `< ShutdownTimeout`. JSM tenant
        webhook timeout is 30s; 20s gives ~10s headroom for the 504
        round-trip.
  - [x] `IdentityProviderID string` (`WEBHOOK_CR_IDENTITY_PROVIDER_ID`,
        **required when JSM provider enabled**) — static IdP identifier
        stamped into every CR's `spec.identityProviderId`. One IdP per
        webhookd install.
- [x] Add `Kubeconfig string` (`WEBHOOK_KUBECONFIG`, default empty —
      empty means in-cluster config).
- [x] Add a top-level `EnabledProviders []string` (`WEBHOOK_PROVIDERS`,
      default `["jsm"]`, comma-separated). Required JSM/CR fields are
      validated only when `"jsm"` is in the list. Self-describing config
      that future providers opt in the same way.
- [x] Update `internal/config/config_test.go` with table-driven cases:
  - [x] All defaults applied when no env vars set (and JSM disabled
        via `withBaselineEnv` helper).
  - [x] All overrides parsed correctly (custom timeout, custom
        namespace, etc.).
  - [x] JSM enabled + missing `WEBHOOK_JSM_FIELD_PROVIDER_GROUP_ID` →
        `ErrJSMFieldsRequired` (parametrized over each required field).
  - [x] JSM enabled + missing `WEBHOOK_CR_IDENTITY_PROVIDER_ID` →
        `ErrIdentityProviderIDRequired`.
  - [x] `WEBHOOK_CR_SYNC_TIMEOUT >= WEBHOOK_SHUTDOWN_TIMEOUT` →
        `ErrSyncTimeoutTooLong` (so we never let JSM time out before
        shutdown drains).
- [x] Update README §Configuration table with every new variable.

#### Success Criteria

- `go test ./internal/config/...` passes with `-race`; coverage stays
  ≥90%.
- All new `WEBHOOK_*` vars appear in the `Config` struct, in test
  coverage, and in README.
- A startup with JSM enabled but missing required field IDs fails with
  a single, clear error message naming the variable.

---

### Phase 2: Provider Interface & Action Union

`internal/webhook` — introduce the small interface surface the dispatcher
will use. No JSM-specific code yet; this phase establishes the seam and
moves the existing Phase 1 handler aside cleanly.

#### Tasks

- [x] Create `internal/webhook/provider.go` defining the `Provider`
      interface exactly as DESIGN-0002 §Provider Interface specifies:
      `Name()`, `VerifySignature(r, body)`, `Handle(ctx, body) (Action, error)`.
- [x] Create `internal/webhook/action.go`:
  - [x] `Action` interface with the unexported sentinel `isAction()`
        method (typed-union pattern; prevents external types from
        masquerading as actions).
  - [x] `NoopAction{Reason string}`.
  - [x] `ApplySAMLGroupMapping{IssueKey string; Spec wizapi.SAMLGroupMappingSpec}`
        where `Spec` carries `IdentityProviderID`, `ProviderGroupID`,
        `Description`, `RoleRef.Name`, `ProjectRefs[0].Name` (single-
        element list per Phase 2 cardinality: one ticket = one CR
        with one project and one role).
  - [x] Sentinel errors `ErrBadRequest` / `ErrUnprocessable` for
        provider returns; the dispatcher classifies via `errors.Is`.
- [x] Create `internal/webhook/result.go`:
  - [x] `ExecResult` struct with `Kind ResultKind, Reason string,
        CRName, Namespace, ObservedGeneration int64`.
  - [x] `ResultKind` enum: `ResultNoop`, `ResultReady`,
        `ResultTransientFailure`, `ResultBadRequest`,
        `ResultUnprocessable`, `ResultInternalError`, `ResultTimeout`.
        (No `ResultTerminalFailure` — operator status is binary;
        Ready=False at watch time falls through to ResultTimeout.)
  - [x] `HTTPStatus()` method on `ResultKind` mapping each kind to
        the DESIGN-0002 §HTTP Response Contract status code, plus
        `String()` for label-safe metric values.
- [x] Migrate Phase 1's `internal/webhook/handler.go`:
  - [x] **Delete** `handler.go` and `handler_test.go`. The
        dispatcher (Phase 6) replaces them.
  - [x] **Keep** `signature.go` and `signature_test.go` as-is — the
        `Verify*` helpers are reused by the JSM provider's
        `VerifySignature` for any v0:-style HMAC.
  - [x] Update `cmd/webhookd/main.go` to register a "tombstone" 503
        handler at `POST /webhook/{provider}` (with
        `Retry-After: 30`) until Phase 6 wires the dispatcher.
- [x] Add a tiny `Provider` mock helper in
      `internal/webhook/providertest/` (`Mock{NameValue, VerifyFunc,
      HandleFunc}`) so the dispatcher tests in Phase 6 don't have to
      spin up JSM. Compile-time check that `*Mock` satisfies
      `webhook.Provider`.

#### Success Criteria

- `go build ./...` succeeds; `go test ./...` passes (with the Phase 1
  handler tests removed).
- `internal/webhook/handler.go` is gone; `internal/webhook/signature.go`
  remains unchanged and its tests pass.
- The new types compile and a unit test asserts `NoopAction` and
  `ApplySAMLGroupMapping` both implement `Action`.

---

### Phase 3: K8s Client & Scheme

`internal/k8s` — the controller-runtime client construction, scheme
registration, and the small wrapper that the executor will consume. Kept
in its own package so the JSM provider does not depend on it (provider
is pure parser).

#### Tasks

- [x] Create `internal/k8s/scheme.go`:
  - [x] Package-level `Scheme = runtime.NewScheme()`.
  - [x] `init()` calling `clientgoscheme.AddToScheme(Scheme)` and
        `wizapi.AddToScheme(Scheme)` via `utilruntime.Must`.
- [x] Create `internal/k8s/client.go`:
  - [x] `NewClients(cfg *config.Config) (*Clients, error)` returns a
        `Clients{CtrlClient, Clientset, RESTConfig}` value combining
        both flavors built from one `*rest.Config`.
  - [x] If `cfg.Kubeconfig != ""` → `clientcmd.BuildConfigFromFlags`.
  - [x] Else → `ctrl.GetConfig()` (honors in-cluster + `KUBECONFIG`
        host env).
  - [x] `client.New(restConfig, client.Options{Scheme: k8s.Scheme})`.
  - [x] Wrap connection errors with `fmt.Errorf("k8s config: %w",
        err)` / `"k8s client: %w"` / `"k8s clientset: %w"`.
  - [x] Startup sanity check: `cfg.CR.APIGroup` must equal
        `wizapi.GroupVersion.Group`; mismatch is fail-fast (typed
        client uses imported GVK; runtime override would silently
        miss the operator).
- [x] Folded `clientset.go` into `client.go` — both flavors come
      from the same `NewClients` entry point so callers can't pick up
      one without the other. `Clients{CtrlClient, Clientset,
      RESTConfig}` is the single struct downstream packages consume.
- [x] Tests in `internal/k8s/client_test.go`:
  - [x] `NewClients` with a non-existent kubeconfig path →
        `"k8s config:" %w` error.
  - [x] `NewClients` with a CR.APIGroup mismatched against
        `wizapi.GroupVersion.Group` → typed mismatch error.
  - [x] Scheme recognizes `SAMLGroupMapping` / `Project` /
        `UserRole` plus core ConfigMap/Namespace.
  - [x] Live-cluster smoke test deferred to Phase 4 envtest.

#### Success Criteria

- `go test ./internal/k8s/...` passes with `-race`.
- `internal/k8s.Scheme` recognizes both core types and SAMLGroupMapping.
- `NewClient` is the only place the project chooses between
  in-cluster and kubeconfig — no other package calls
  `ctrl.GetConfig()` directly.

---

### Phase 4: Executor (K8s Apply + Sync Watch)

`internal/webhook/executor.go` — the side-effectful half of the pipeline.
The executor receives an `Action` and returns an `ExecResult`. It owns
the SSA call, the watch loop, and classification of K8s errors at apply
time. **The watch step is binary** — `Ready=True` is success, anything
else (including `Ready=False` with any reason) is transient. The Wiz API
the operator talks to is binary too, so there is no terminal/transient
classification at the watch step. Pre-validation of project/role
references against existing CRs is deferred to a future JSM-specific
middleware.

#### Tasks

- [x] Create `internal/webhook/executor.go`:
  - [x] `Executor` struct holding `client.WithWatch`, `*slog.Logger`,
        and a narrow `ExecutorConfig`
        (`Namespace, FieldManager, SyncTimeout, Now`). The original
        plan called for both `client.Client` + `*kubernetes.Clientset`,
        but the watch path needed `client.WithWatch` anyway and the
        clientset turned out unused — single typed client is simpler.
  - [x] `Execute(ctx, Action) ExecResult`:
    - [x] Switch on action type.
    - [x] `NoopAction` → `ResultNoop` with reason copied through.
    - [x] `ApplySAMLGroupMapping` → `apply(...)` → `waitForSync(...)`.
- [x] Implement `apply(ctx, *ApplySAMLGroupMapping) (*wizapi.SAMLGroupMapping, error)`:
  - [x] Build the typed object: TypeMeta (group `wiz.webhookd.io`,
        version `v1alpha1`, kind `SAMLGroupMapping`), ObjectMeta (name =
        `crName(issueKey)`, labels `webhookd.io/managed-by=webhookd` +
        `webhookd.io/source=jsm`, annotations
        `webhookd.io/trace-id`, `webhookd.io/request-id`,
        `webhookd.io/jsm-issue-key`, `webhookd.io/applied-at`).
  - [x] `client.Patch(ctx, obj, client.Apply,
        client.FieldOwner(cfg.FieldManager), client.ForceOwnership)`.
  - [x] After Patch returns, refetch via `client.Get` so we have the
        current `metadata.generation`.
  - [ ] Span attributes (`k8s.apply` span with `k8s.resource.*` and
        `webhookd.outcome`) — deferred to Phase 7 (Observability
        Additions); the executor stays observability-agnostic for now.
- [x] Implement `crName(issueKey string) string`:
  - [x] Lowercase, replace any non-`[a-z0-9-]` with `-`,
        prefix with `jsm-`.
- [x] Implement `annotations(ctx, IssueKey) map[string]string` that
      reads request-id from context (Phase 1's
      `httpx.RequestIDFromContext`) and trace-id from
      `trace.SpanFromContext(ctx).SpanContext().TraceID().String()`.
- [x] Implement `waitForSync(ctx, applied) ExecResult`:
  - [x] `ctx, cancel := context.WithTimeout(ctx, cfg.SyncTimeout); defer cancel()`.
  - [x] Initial `client.Get` to close the Patch→Watch race window.
  - [x] Single namespace-scoped `client.WithWatch.Watch()` + manual
        event loop. We deliberately don't use
        `cache.Reflector` / `tools/watch.UntilWithSync` — those depend
        on streaming-list bookmarks (`WatchListClient` feature gate,
        default-on in client-go v0.35+) that a custom ListWatch
        can't supply, and our hard `SyncTimeout` deadline obviates
        Reflector's auto-reconnect logic. **This is a deviation from
        the original plan**, captured in IMPL-0002 §Resolved
        Decisions and worth surfacing in a follow-up ADR.
  - [x] Per-event predicate (`isReady`) — **binary**:
    - [x] If `obj.Status.ObservedGeneration < applied.Generation` →
          `false` (operator hasn't seen this gen yet).
    - [x] Find `Ready` condition. If absent → `false`.
    - [x] If `Ready == True` → `true` (caller maps to `ResultReady`).
    - [x] Otherwise → `false` (Ready=False or Unknown is treated as
          still-pending until the deadline).
  - [x] After watch loop returns:
    - [x] `ctx.Err() == DeadlineExceeded` → `ResultTimeout` (504 to
          JSM; CR may still be Ready=False — that's OK, JSM retries).
    - [x] Watch channel closed before sync → `ResultTransientFailure`
          (logged at warn).
    - [x] Predicate matched → `ResultReady` with observedGeneration.
- [x] Implement `classifyK8sErr(err error) ExecResult` for SSA call
      failures (apply-step, deterministic):
  - [x] `apierrors.IsForbidden(err)` → `ResultInternalError` (500;
        RBAC bug — fail loudly, don't degrade to transient).
  - [x] `apierrors.IsInvalid(err)` → `ResultUnprocessable` (422; CRD
        schema violation — caller's spec is wrong, retry won't help).
        Note: §Resolved Decisions says `ResultBadRequest` (400)
        but `Unprocessable` (422) better matches DESIGN-0002's
        contract: a syntactically well-formed payload that is
        semantically rejectable; retry won't help.
  - [x] `apierrors.IsServerTimeout(err)`,
        `apierrors.IsServiceUnavailable(err)`,
        `apierrors.IsTooManyRequests(err)`,
        `apierrors.IsConflict(err)` → `ResultTransientFailure`.
  - [x] Default → `ResultTransientFailure` with the K8s error wrapped.
- [x] Tests in `internal/webhook/executor_test.go` (envtest required):
  - [x] Spin up an envtest control plane in `TestMain`. Install the
        SAMLGroupMapping CRD from `deploy/crds/` (Project + UserRole
        not needed for executor tests; deferred to Phase 8 fixtures).
  - [x] Happy path: apply, then in a goroutine patch
        `status.conditions[Ready]=True` and bump observedGeneration.
        Assert `ResultReady`.
  - [x] Timeout path: apply, never patch status. Assert `ResultTimeout`.
  - [x] Ready=False is transient: patch
        `status.conditions[Ready]={False, Reason: OperatorReconciling}`.
        Assert `ResultTimeout` — *not* a 4xx-mapped kind. Codifies the
        "binary watch" contract.
  - [x] SSA invalid spec: apply with empty `IdentityProviderID`
        (violates CRD's `minLength: 1`). Assert `ResultUnprocessable`
        (422 path).
  - [ ] SSA forbidden: deferred — envtest's default cluster-admin
        kubeconfig makes this hard to simulate without a separate user
        binding. Covered by `TestClassifyK8sErr/forbidden` synthesizing
        an `apierrors.NewForbidden` directly.
  - [x] Idempotency: apply twice with identical spec + pinned `Now`;
        assert second Get returns the same generation as the first.
  - [ ] SSA conflict with `ForceOwnership` (different fieldManager
        takes ownership): deferred — same reasoning as forbidden;
        controller-runtime's `ForceOwnership` semantics are exercised
        in upstream tests, and our happy-path test confirms
        `client.ForceOwnership` doesn't error.
- [x] Skip `goleak.VerifyTestMain` for envtest — envtest leaks
      transport-pool goroutines that aren't worth maintaining
      `IgnoreTopFunction` allowlists for, and `cmd/webhookd`'s
      integration test already exercises the no-leak invariant for
      production code.

#### Success Criteria

- `go test ./internal/webhook/... -race` passes; envtest tests run
  inside `make test` (gated by `KUBEBUILDER_ASSETS`).
- Coverage on `internal/webhook` (executor + helpers) ≥80%.
- Each documented success/timeout/transient/apply-error path is
  exercised by at least one envtest case.
- `classifyK8sErr` maps every documented K8s error class to the
  expected `ResultKind`.
- `Ready=False` from the operator is treated as still-pending (waits
  for the full timeout budget), not as terminal failure.

---

### Phase 5: JSM Provider

`internal/webhook/jsm` — the first concrete `Provider`. Pure parsing +
spec construction; no K8s, no HTTP outside what the Provider interface
hands in. Highly unit-testable.

#### Tasks

- [x] Create `internal/webhook/jsm/payload.go`:
  - [x] `Payload` struct mirroring DESIGN-0002 §JSM Webhook Payload:
        `Issue.Key`, `Issue.Fields.Status.Name`, plus a
        `map[string]json.RawMessage` for custom fields populated by a
        custom UnmarshalJSON on `IssueFields`.
  - [x] `Decode(body []byte) (*Payload, error)` returning typed errors
        (`ErrInvalidJSON`, `ErrMissingIssue`, `ErrMissingIssueKey`,
        `ErrMissingStatus`).
- [x] Create `internal/webhook/jsm/extract.go`:
  - [x] `ExtractString(p *Payload, fieldID string) (string, error)`.
  - [x] Typed errors: `ErrFieldMissing`, `ErrFieldEmpty`, `ErrFieldType`.
- [x] Create `internal/webhook/jsm/cr.go`:
  - [x] `BuildSpec(providerGroupID, role, project, identityProviderID,
        description string) wizapi.SAMLGroupMappingSpec`. Single-element
        `ProjectRefs` always.
  - [x] `BuildDescription(issueKey string) string`.
- [x] Create `internal/webhook/jsm/signature.go`:
  - [x] Wraps `internal/webhook.Verify(...)` against the configured
        JSM headers (`SignatureConfig{SecretBytes, SigHeader, TSHeader,
        Skew, Now}`). No JSM-native scheme assumed; v0:<ts>:<body>
        contract reused as-is.
- [x] Create `internal/webhook/jsm/provider.go`:
  - [x] `Provider` struct + `New(cfg *Config) *Provider`. cfg taken
        by pointer to dodge gocritic hugeParam (Signature drags in
        secret + clock + headers).
  - [x] Compile-time `var _ webhook.Provider = (*Provider)(nil)`.
  - [x] `Name() string { return "jsm" }`.
  - [x] `VerifySignature` delegates to package helper.
  - [x] `Handle`: Decode → trigger-status check → extract three custom
        fields → BuildSpec → ApplySAMLGroupMapping.
  - [x] Extract failures classified: missing/empty → ErrBadRequest,
        wrong type → ErrUnprocessable.
  - [ ] Span instrumentation (`jsm.decode_payload`, `jsm.extract_fields`)
        deferred to Phase 7.
- [x] Create `internal/webhook/jsm/response.go`:
  - [x] `ResponseBody` JSON shape: `status`, `reason`, `crName`,
        `namespace`, `observedGeneration`, `traceId`, `requestId`.
  - [x] `Build(res webhook.ExecResult, traceID, requestID) ResponseBody`
        with status mapping (Noop→noop, Ready→success, else failure).
- [x] Tests in `internal/webhook/jsm/*_test.go`:
  - [x] `payload_test.go` — table-driven across all sentinels +
        ignores-unknown-fields case.
  - [x] `extract_test.go` — table-driven across present/missing/null/
        empty/whitespace/non-string-type cases.
  - [x] `cr_test.go` — `BuildSpec` single-element ProjectRefs assertion;
        `BuildDescription` exact output.
  - [x] `signature_test.go` — known-good HMAC vector; wrong-secret,
        replay outside skew, missing timestamp, malformed prefix.
  - [x] `provider_test.go` — Name, Noop on non-trigger status, Apply on
        trigger status with full spec assertion, BadRequest classes,
        Unprocessable on wrong type.
  - [x] `response_test.go` — status-mapping table + identity field
        passthrough + omitempty correctness.
  - [x] `FuzzJSMDecode` seeded with canonical sample + malformed
        variants. 15s clean (full 60s deferred to pre-merge gate).
- [x] `internal/webhook/jsm/testdata/sample.json` — anonymized payload
      for fuzz seeds and Phase 6 integration test.

#### Success Criteria

- `go test ./internal/webhook/jsm/... -race` passes; coverage ≥85%.
- `go test -fuzz=FuzzJSMDecode -fuzztime=60s ./internal/webhook/jsm`
  finds nothing.
- The JSM provider has zero imports of `internal/k8s`,
  `controller-runtime`, or `net/http` server code (only the standard
  `*http.Request` type from `Provider.VerifySignature`). Verified by a
  small import-graph test (or just `go list -deps`).

---

### Phase 6: Dispatcher & Application Wiring

`internal/webhook/dispatcher.go` plus updates to
`cmd/webhookd/main.go`. This is where everything composes: the
dispatcher routes by path-value, the executor applies, the response
shape goes back to JSM.

#### Tasks

- [x] Create `internal/webhook/dispatcher.go`:
  - [x] `Dispatcher` struct holding
        `providers map[string]Provider`, `responseBuilder ResponseBuilder`,
        `executor executorIface`, `logger *slog.Logger`, `maxBodyBytes int64`.
        Original plan called for functional options; switched to a
        struct (`DispatcherConfig`) because the field set is small and
        stable — functional options would have been ceremony.
  - [x] `NewDispatcher(cfg DispatcherConfig) *Dispatcher`.
  - [x] `ResponseBuilder` interface added so each provider supplies
        its own response shape; `*jsm.Provider` implements it via
        `BuildResponse(res, traceID, requestID) any` (returning
        `jsm.ResponseBody`). When a second provider lands this stays
        as one-builder-per-provider rather than a type switch.
  - [x] `ServeHTTP(w, r)`:
    - [x] `r.PathValue("provider")` lookup → 404 on miss.
    - [x] `http.MaxBytesReader` with 413 / 400 classification.
    - [x] `prov.VerifySignature(r, body)` → 401.
    - [x] `prov.Handle` → on error, `classifyProviderErr` maps to
          ResultBadRequest / ResultUnprocessable / ResultInternalError.
    - [x] `executor.Execute` → ExecResult.
    - [x] `writeResponse` writes status from `Kind.HTTPStatus()`,
          body from `responseBuilder.BuildResponse(res, traceID, requestID)`.
  - [x] Duplicate provider-name registration panics at construction.
- [x] Update `cmd/webhookd/main.go`:
  - [x] After observability: `k8s.NewClients(cfg)` (gated on
        `slices.Contains(cfg.EnabledProviders, "jsm")`).
  - [x] Build executor with `webhook.NewExecutor(clients.CtrlClient,
        logger, ExecutorConfig{...})`.
  - [x] Build JSM provider with `jsm.New(&jsm.Config{...})` reading
        from `cfg.JSM` and `cfg.CR`.
  - [x] Build dispatcher; mount at `POST /webhook/{provider}`.
  - [x] When JSM is disabled, fall back to a 503 tombstone so Phase 1
        integration tests keep working without K8s.
  - [x] Phase 1 middleware chain unchanged (Recover, OTel, RequestID,
        SLog, Metrics, RateLimit).
- [x] Tests in `internal/webhook/dispatcher_test.go` (no envtest —
      uses `providertest.Mock`):
  - [x] Unknown provider → 404.
  - [x] Body too large → 413.
  - [x] Bad signature → 401.
  - [x] Provider returns NoopAction → 200 + `kind: "noop"`.
  - [x] Provider returns BadRequest / Unprocessable / generic error
        → 400 / 422 / 500 with matching kind.
  - [x] Executor returns each `ResultKind` → expected status code per
        DESIGN-0002 §HTTP Response Contract.
  - [x] Duplicate provider names panics.
- [x] Integration test in `cmd/webhookd/main_test.go`
      (`TestRun_EndToEnd_JSMToReadyCR`):
  - [x] envtest started in TestMain (gated on `KUBEBUILDER_ASSETS`),
        SAMLGroupMapping CRD installed from `deploy/crds/`.
  - [x] envtest's *rest.Config materialized to a kubeconfig file via
        `clientcmd.WriteToFile` so `WEBHOOK_KUBECONFIG` can point at
        it.
  - [x] Real `run(ctx)` boots in a goroutine; signed JSM payload
        POSTed to `/webhook/jsm`.
  - [x] Operator-impersonator goroutine writes Ready=True on the CR.
  - [x] Asserts 200 + `status: "success"` + `crName: "jsm-sec-9001"`.
  - [x] `goleak.Find` runs after envtest.Stop with IgnoreTopFunction
        for envtest's transport-pool / wait-loop residuals.
  - [ ] Metric assertions on `webhookd_k8s_apply_total` /
        `webhookd_k8s_sync_duration_seconds` — deferred to Phase 7
        (the metrics themselves haven't shipped yet; they land in
        Phase 7 alongside the trace-id ADR).

#### Success Criteria

- `go test ./... -race` passes including the envtest integration
  test in `cmd/webhookd/`.
- A locally-running webhookd with a valid kubeconfig serves
  `POST /webhook/jsm` end-to-end against an envtest cluster.
- The Phase 1 receiver behavior (`/healthz`, `/readyz`, `/metrics`,
  rate limiting, request-id propagation, trace correlation) is
  unchanged — verified by re-running the Phase 1 integration test
  unchanged.

---

### Phase 7: Observability Additions

Backfill the new metrics on the `Metrics` struct, attach span
attributes consistently, and document the cross-boundary trace
propagation contract for the operator team.

#### Tasks

- [ ] In `internal/observability/metrics.go`, register on the same
      private `prometheus.Registry` (note: `_k8s_` prefix instead of
      `_cr_` for forward compatibility with future K8s ops like the
      ref-validation lookups):
  - [ ] `K8sApplyTotal *prometheus.CounterVec` →
        `webhookd_k8s_apply_total{kind, outcome}` — outcome:
        `created|updated|unchanged|error`.
  - [ ] `K8sSyncDuration *prometheus.HistogramVec` →
        `webhookd_k8s_sync_duration_seconds{kind, outcome}` —
        outcome: `ready|timeout|transient`; buckets `0.1, 0.25, 0.5,
        1, 2, 5, 10, 20, 30`.
  - [ ] `JSMPayloadParseErrors *prometheus.CounterVec` →
        `webhookd_jsm_payload_parse_errors_total{reason}` — reason:
        `invalid_json|missing_field|wrong_type|empty_field`.
  - [ ] `JSMNoopTotal *prometheus.CounterVec` →
        `webhookd_jsm_noop_total{trigger_status}`.
  - [ ] `JSMResponseTotal *prometheus.CounterVec` →
        `webhookd_jsm_response_total{status_code}`.
- [ ] Update `internal/observability/metrics_test.go` with scrape
      assertions that all five new instruments appear in the
      exposition (after a single observation per child to avoid the
      Phase 1 "vec with no children" gotcha documented in
      `CLAUDE.md`).
- [ ] In the executor's `apply` and `waitForSync`, record on the
      relevant counters/histograms with the right labels.
- [ ] In the JSM provider, record on `JSMPayloadParseErrors` and
      `JSMNoopTotal`.
- [ ] In the dispatcher, record on `JSMResponseTotal` with the
      written status code.
- [ ] Verify span set against DESIGN-0002 §Observability Additions:
  - [ ] `jsm.decode_payload`, `jsm.extract_fields` — created in the
        JSM provider with attributes `jsm.issue_key`,
        `jsm.provider_group_id` (no `jsm.field_format` — Phase 2
        doesn't have a format strategy).
  - [ ] `k8s.apply` — created in executor with attributes
        `k8s.resource.kind`, `k8s.resource.name`,
        `k8s.resource.namespace`, `k8s.generation`,
        `webhookd.outcome`.
  - [ ] `k8s.watch_cr` — created in executor with `k8s.sync.outcome`
        set on close.
  - [ ] `jsm.build_response` — created in the dispatcher
        response-write path.
- [ ] Update README §Observability with a paragraph on Phase 2 and
      a table listing the new metrics.
- [ ] Add **ADR-0007: Trace context propagation via CR annotation**
      at `docs/adr/0007-trace-context-propagation-via-cr-annotation.md`
      documenting the contract: webhookd writes the W3C trace-id
      (32-hex-char) to the `webhookd.io/trace-id` annotation on every
      applied CR; the operator's reconciler reads that annotation,
      builds a remote-parent `SpanContext`, and links its reconcile
      span as a child. This gives Tempo a single trace spanning the
      JSM → webhookd → operator → Wiz path. ADR cites
      DESIGN-0002 §Observability for rationale.

#### Success Criteria

- A scrape of `/metrics` after a single ticket flow shows all five
  new `webhookd_*` metrics with the right labels and at least one
  observation each.
- A trace exported during the integration test contains all five
  new span names as children of the `otelhttp` server span; the
  applied CR carries `webhookd.io/trace-id` matching the trace
  ID.

---

### Phase 8: RBAC, Sample Manifests, Documentation

Land the artifacts a deployer needs to actually run Phase 2 in a
cluster: RBAC, a sample CRD (or pointer at the operator's), and the
README updates that turn this from "code merged" into "shippable."

#### Tasks

- [ ] Create `deploy/rbac/role.yaml` (Namespaced Role in
      `wiz-operator` namespace) with verbs `get`, `list`, `watch`,
      `patch` on `wiz.webhookd.io/samlgroupmappings`. (No verbs on
      `projects` or `userroles` for Phase 2 — pre-validation
      middleware in a future phase will add `get` on those.)
- [ ] Create `deploy/rbac/rolebinding.yaml` binding the Role to the
      `webhookd` ServiceAccount in the webhookd namespace.
- [ ] Create `deploy/rbac/serviceaccount.yaml` for completeness.
- [ ] Add `deploy/crds/` *fixtures* (used by envtest) carrying the
      minimal CRD definitions that match the Go types: one each for
      `samlgroupmapping.yaml`, `project.yaml`, `userrole.yaml`. All
      clearly labeled `# fixture only; canonical CRDs ship with the
      operator project`. The Phase 8 sample YAMLs in
      `docs/examples/samples/` are user-facing references; the
      `deploy/crds/` fixtures are envtest-only (minimal schemas,
      validate the Go types load).
- [ ] Update `README.md`:
  - [ ] Status line: Phase 2 is now Implemented (when this lands).
  - [ ] Add a §"JSM provider" subsection documenting how the
        webhook is configured on the JSM side, what the response
        body looks like, and the failure-mode → JSM behavior table.
  - [ ] Add a §"Deployment" subsection pointing at `deploy/rbac/`
        and listing the Phase 2 env vars.
- [ ] Update `CLAUDE.md`:
  - [ ] Project-state paragraph: Phase 2 implemented.
  - [ ] Architectural patterns: capture the
        "Provider parses → Executor side-effects" split as a
        durable pattern future providers must follow.
  - [ ] Testing patterns: record any new gotchas surfaced by
        envtest (CRD install ordering, etcd cleanup, anything
        else).
- [ ] Flip `docs/design/0002-jsm-webhook-to-samlmapping-provisioning-phase-2.md`
      status to `Implemented` once Phase 6's integration test
      lands. Run `docz update` to refresh indexes.
- [ ] Flip this doc's `status:` to `Complete` and update the
      Resolved Decisions section (mirror IMPL-0001's pattern) with
      the answers to the Open Questions below.

#### Success Criteria

- `kubectl apply -f deploy/rbac/` against an envtest or kind
  cluster succeeds and grants webhookd the documented verbs.
- README and CLAUDE.md reflect the post-Phase-2 reality;
  `docz update` reports clean indexes.
- DESIGN-0002 status flipped to Implemented; IMPL-0002 status
  flipped to Complete with all task boxes checked.

---

## File Changes

| File | Action | Description |
|------|--------|-------------|
| `go.mod`, `go.sum` | Modify | Add controller-runtime, operator API, envtest. |
| `Makefile` | Modify | Add `tools-envtest` target and `KUBEBUILDER_ASSETS` wiring for `make test`. |
| `internal/config/config.go` | Modify | `JSMConfig` (TriggerStatus, FieldProviderGroupID, FieldRole, FieldProject), `CRConfig` (Namespace, APIGroup default `wiz.webhookd.io`, APIVersion, FieldManager, SyncTimeout, IdentityProviderID), `EnabledProviders`, `Kubeconfig`. |
| `internal/config/config_test.go` | Modify | New cases for the additions. |
| `internal/k8s/scheme.go` | Create | Single shared `runtime.Scheme`. |
| `internal/k8s/client.go` | Create | `NewClient` wrapper around `ctrl.GetConfig` + `client.New`. |
| `internal/k8s/clientset.go` | Create | `*kubernetes.Clientset` for watch-tools usage. |
| `internal/k8s/*_test.go` | Create | Unit tests on scheme + client construction. |
| `internal/webhook/wizapi/types.go` | Create *(temporary)* | Local stub of operator API types (`SAMLGroupMapping`, `Project`, `UserRole`, `GroupVersion = wiz.webhookd.io/v1alpha1`, `AddToScheme`) until upstream module is consumable. Deleted once `github.com/donaldgifford/wiz-operator/api/v1alpha1` is importable. |
| `internal/webhook/handler.go` | **Delete** | Replaced by dispatcher. |
| `internal/webhook/handler_test.go` | **Delete** | — |
| `internal/webhook/signature.go` | Modify *(maybe)* | Untouched if same-package; small reorg if a sigutil split is needed. |
| `internal/webhook/provider.go` | Create | `Provider` interface. |
| `internal/webhook/action.go` | Create | `Action` union. |
| `internal/webhook/result.go` | Create | `ExecResult`, `ResultKind`, status mapping. |
| `internal/webhook/dispatcher.go` | Create | HTTP dispatcher. |
| `internal/webhook/dispatcher_test.go` | Create | Mock-provider table-driven tests. |
| `internal/webhook/executor.go` | Create | SSA apply + watch. |
| `internal/webhook/executor_test.go` | Create | envtest integration tests. |
| `internal/webhook/providertest/mock.go` | Create | Test-only fake `Provider`. |
| `internal/webhook/jsm/payload.go` | Create | JSON shape + decode. |
| `internal/webhook/jsm/extract.go` | Create | Custom-field extraction. |
| `internal/webhook/jsm/cr.go` | Create | Spec builder. |
| `internal/webhook/jsm/signature.go` | Create | JSM HMAC verify wrapper. |
| `internal/webhook/jsm/provider.go` | Create | `Provider` impl. |
| `internal/webhook/jsm/response.go` | Create | Response body shape + builder. |
| `internal/webhook/jsm/*_test.go` | Create | Unit + fuzz tests. |
| `internal/webhook/jsm/testdata/sample.json` | Create | Anonymized JSM sample. |
| `internal/observability/metrics.go` | Modify | Five new instruments. |
| `internal/observability/metrics_test.go` | Modify | Scrape assertions. |
| `cmd/webhookd/main.go` | Modify | Wire k8s client → executor → dispatcher → mux. |
| `cmd/webhookd/main_test.go` | Modify | Replace Phase 1 happy-path with full envtest end-to-end. |
| `deploy/rbac/{role,rolebinding,serviceaccount}.yaml` | Create | Sample RBAC manifests. |
| `deploy/crds/{samlgroupmapping,project,userrole}.yaml` | Create | envtest CRD fixtures (operator owns canonical). |
| `docs/examples/samples/*.yaml` | Modify *(done)* | API group changed from `wiz.rtkwlf.io` to `wiz.webhookd.io`. |
| `docs/adr/0007-trace-context-propagation-via-cr-annotation.md` | Create | Trace-id annotation contract for cross-boundary tracing. |
| `README.md`, `CLAUDE.md` | Modify | Phase 2 status + JSM/deployment sections. |

## Testing Plan

- **Unit tests** — pure-parser focus in `internal/webhook/jsm` and
  pure-mapper focus in `internal/webhook` (action/result helpers).
  Table-driven, stdlib `testing`, `-race` enabled. Coverage targets:
  `jsm` ≥85%, `webhook` ≥80%, `k8s` ≥80%, `config` stays ≥90%.
- **Fuzz target** — `FuzzJSMDecode` on the payload decoder, seeded
  from `testdata/sample.json` plus deliberately malformed variants.
  Required to run 60+ seconds clean before merge (mirrors Phase 1).
- **Integration tests (envtest)** — Phase 4 covers executor in
  isolation; Phase 6 covers the full pipeline. Both run inside
  `make test` once `KUBEBUILDER_ASSETS` is set; CI installs it via
  `make tools-envtest`.
- **End-to-end (manual, pre-release)** — kind cluster + real Wiz
  operator + (recorded or sandbox) Wiz API; out of CI by design.
  Recorded as a checklist in `docs/runbook/release-checklist.md`
  when written.
- **Negative-path coverage** — explicit envtest cases for: SSA
  forbidden (RBAC denies patch → 500), CRD invalid (spec doesn't
  conform → 422 from `IsInvalid`), operator never reconciles
  (timeout → 504), operator marks `Ready=False` (still pending →
  504 after the watch budget — no terminal/transient classification
  at the watch step).

## Dependencies

Direct module imports introduced by this implementation:

- `sigs.k8s.io/controller-runtime` — typed `client.Client`, `ctrl.GetConfig`.
- `k8s.io/apimachinery` — runtime/scheme, watch package, condition helpers.
- `k8s.io/client-go` — `kubernetes.Clientset`, `cache.ListWatch`,
  `tools/watch.UntilWithSync`, `clientcmd`.
- Operator API module: `github.com/donaldgifford/wiz-operator/api/v1alpha1`
  — `SAMLGroupMapping{Spec,Status}`, `Project`, `UserRole`,
  `GroupVersion = wiz.webhookd.io/v1alpha1`, `AddToScheme`. *The
  operator repo does not exist yet (2026-04-27); Phase 0 ships against
  the local `internal/webhook/wizapi` stub. Swap to the real module is
  one alias change.*
- `sigs.k8s.io/controller-runtime/pkg/envtest` (test-only) — in-process
  K8s API server.

No new runtime deps beyond the K8s ecosystem. No JSM SDK — the JSM
side is "they POST signed JSON, we parse." No callback path → no JSM
REST credentials.

External prerequisites:

- Running clusters that webhookd will deploy to have the Wiz
  operator and the SAMLGroupMapping / Project / UserRole CRDs
  installed before webhookd starts — webhookd does not bootstrap
  the CRDs.
- envtest binary set is installable in CI (the existing CI
  pipelines already build under Linux; `setup-envtest` resolves a
  matching binary set per Kubernetes version).

## Resolved Decisions

These started as open questions; the answers below are now baked into
the phase tasks above. Kept here so future readers can see the
reasoning rather than just the outcome (mirrors IMPL-0001's pattern).

1. **Operator API Go module path —
   `github.com/donaldgifford/wiz-operator/api/v1alpha1`.** The repo
   does not exist yet (as of 2026-04-27). Phase 0 ships against a
   local `internal/webhook/wizapi/` stub carrying `SAMLGroupMapping`,
   `Project`, `UserRole` types + `GroupVersion =
   wiz.webhookd.io/v1alpha1` + `AddToScheme`. When the operator repo
   is published, the swap is one alias change in `wizapi.go` plus
   stub deletion.
2. **CRD shape locked from `docs/examples/samples/`.** Kind
   `SAMLGroupMapping` (not `SAMLMapping`); group `wiz.webhookd.io`
   (renamed from `wiz.rtkwlf.io` — samples updated in this session).
   Spec carries `identityProviderId` (static config),
   `providerGroupId` (from JSM), `description` (derived: "Provisioned
   from JSM `<key>`"), `roleRef.name` (from JSM, references a
   `UserRole` CR), `projectRefs[0].name` (from JSM, references a
   `Project` CR). Cardinality: 1 JSM ticket = 1 CR with one project
   and one role. Refs are name-based for Phase 2; Wiz-ID alternatives
   deferred. `Project` and `UserRole` CRs pre-exist out-of-band —
   webhookd never creates them. (Future enhancement: singular
   `projectRef` instead of `projectRefs[]` on the operator side
   would align 1:1 with one ticket = one CR more cleanly.)
3. **Operator status signal is binary; no condition-reason
   taxonomy.** The Wiz API gives the operator no way to distinguish
   "permanent failure" from "Wiz had a bad day." So webhookd
   classifies only at the apply step (deterministic K8s errors:
   `IsForbidden` → 500, `IsInvalid` → 422, others → 503) and treats
   every non-`Ready=True` watch outcome as still-pending until the
   timeout deadline (504). Pre-validation of project/role refs (a
   real terminal-vs-transient signal) is deferred to a future
   JSM-specific middleware that calls a separate API.
4. **JSM tenant timeout 30s; `WEBHOOK_CR_SYNC_TIMEOUT=20s` default.**
   ~10s headroom for the 504 round-trip and JSM connection-handling.
   Tune from `webhookd_k8s_sync_duration_seconds` p95/p99 after the
   first week of real traffic.
5. **Delete Phase 1 `handler.go` + `handler_test.go`.** The
   dispatcher replaces them. Two routing models would be one too
   many; the fallback path would never be tested. `signature.go` and
   its tests stay — JSM reuses the v0: HMAC helpers.
6. **Dual-client watch via `tools/watch.UntilWithSync`.** Phase 3
   exposes both `client.Client` (controller-runtime, for typed
   apply) and `*kubernetes.Clientset` (client-go, for the
   `cache.ListWatch` that `UntilWithSync` consumes). Re-list-on-
   disconnect semantics come from `UntilWithSync` for free; the
   alternative (write our own loop on `client.Watch`) reimplements
   work that already exists. Single shared `rest.Config`; no
   runtime overhead.
7. **`make tools-envtest` shells to `setup-envtest`.** Standard
   kubebuilder pattern. `setup-envtest use <k8s-version>` fetches
   binaries to `bin/k8s/<version>/`; `make test` exports
   `KUBEBUILDER_ASSETS=$(setup-envtest use -p path <version>)`. No
   mise plugin dependency.
8. **ADR-0007 for trace-id annotation contract.** New ADR documents
   the cross-boundary contract (webhookd writes
   `webhookd.io/trace-id` on every CR; operator reads it and links
   its reconcile span as a remote-parent child). ADRs survive
   design-doc lifecycle changes; the operator team has a stable URL
   to cite.
9. **Metric prefix `webhookd_k8s_*`.** DESIGN-0002 originally said
   `_cr_` but `_k8s_` generalizes to future K8s ops (the
   ref-validation middleware lookups, namespace probes, etc.) without
   forcing a Prometheus-metric rename later. `kind` label on each
   metric distinguishes `SAMLGroupMapping` from future operations.
10. **Stub-and-swap for the operator API.** Confirmed in #1. Local
    stub for Phase 0; mechanical swap when the operator repo is
    published. No CRD-to-types codegen — the stub is hand-written
    against the YAML samples, which is fast at this size.
11. **`WEBHOOK_PROVIDERS=jsm` default; gates JSM-required config.**
    Mirrors the `WEBHOOK_PPROF_ENABLED` knob pattern from Phase 1.
    Self-describing config; future providers (`slack`, etc.) opt in
    the same way.
12. **`WEBHOOK_MAX_BODY_BYTES=1MiB` unchanged.** Real JSM payloads
    are 5–50 KB; 1 MiB is 20× headroom. If a real ticket trips 413,
    fix is one env-var override in the deployment manifest — no
    code change.

### Cross-doc follow-ups

- **DESIGN-0002 still has the original strawman CR shape**
  (`SAMLMapping` kind, `wiz.example.com` group, `team` + `projects[]`
  spec). It will be updated to match the new shape as part of Phase 8
  when its status flips to Implemented.
- **Annotation prefix change** from `webhookd.wiz.io/...` (used in
  DESIGN-0002 draft) to `webhookd.io/...` (used here). Captured in
  ADR-0007.

## References

- DESIGN-0002 — JSM Webhook → SAMLGroupMapping Provisioning, Phase 2 (the
  source of truth for what to build).
- DESIGN-0001 / IMPL-0001 — Phase 1 receiver substrate this builds on.
- ADR-0004 — controller-runtime typed client for Kubernetes access.
- ADR-0005 — Server-Side Apply for custom resource reconciliation.
- ADR-0006 — Synchronous JSM response contract (no async callback).
- `archive/walk2.md` (gitignored) — line-by-line implementation
  walkthrough for Phase 2; canonical reference for package layout
  and the dispatcher / executor split.
- controller-runtime client docs:
  <https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/client>
- `tools/watch.UntilWithSync`:
  <https://pkg.go.dev/k8s.io/client-go/tools/watch>
- envtest:
  <https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/envtest>
- Kubernetes Server-Side Apply:
  <https://kubernetes.io/docs/reference/using-api/server-side-apply/>
- JSM automation webhooks:
  <https://support.atlassian.com/cloud-automation/docs/jira-automation-triggers/>
