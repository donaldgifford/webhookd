---
id: ADR-0004
title: "controller-runtime typed client for Kubernetes access"
status: Accepted
author: Donald Gifford
created: 2026-04-24
---

<!-- markdownlint-disable-file MD025 MD041 -->

# 0004. controller-runtime typed client for Kubernetes access

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

Phase 2 adds a Kubernetes action path: webhookd applies a `SAMLMapping` custom
resource and watches its status. There are two axes to decide on:

1. **Client library** — raw `client-go` (the official Kubernetes client), or
   `sigs.k8s.io/controller-runtime/pkg/client` (the higher-level client used by
   Kubebuilder operators).
2. **Typing** — typed API objects (`*wizv1alpha1.SAMLMapping`) versus
   dynamic/unstructured objects (`*unstructured.Unstructured`).

The Wiz operator is built with Kubebuilder, uses controller-runtime, and exports
its API types as a Go module. webhookd lives in the same organization, is
written by the same team, and already takes a hard dependency on the operator's
CRD shape via the apply-and-watch contract (DESIGN-0002).

Using raw `client-go` would give us a slightly smaller dependency tree and a
lower-level API (Typed/Dynamic/Discovery clients), but we'd reimplement patterns
(SSA-as-patch, typed watch) that controller-runtime has already stabilized for
the operator codebase.

Using `unstructured.Unstructured` would let webhookd avoid importing the
operator's Go module, which sounds appealing for decoupling. But field typos
(e.g. `spec.projcts` vs `spec.projects`) would then only fail at admission time
on the live API server, not at compile time.

## Decision

Use `sigs.k8s.io/controller-runtime/pkg/client` with the operator's API types
registered in a `runtime.Scheme`. All CR reads, writes, and watches go through
`client.Client` on typed objects.

Client construction uses `ctrl.GetConfig()` so the same binary works in-cluster
(via ServiceAccount) and out-of-cluster (via `KUBECONFIG` env var) without code
changes.

## Consequences

### Positive

- Compile-time safety on every field reference. Typos in `SAMLMappingSpec`
  fields become build errors.
- `client.Patch(ctx, obj, client.Apply, client.FieldOwner(...))` is the one-line
  idiom for Server-Side Apply (see ADR-0005); no hand-rolled patch construction.
- One mental model shared with the operator codebase. Engineers switching
  between the two repos work with the same `client.Client`, `client.Patch`,
  `client.List`, etc.
- Watch-with-list (`watch.UntilWithSync`) and typed informers compose cleanly
  with the typed scheme.

### Negative

- Hard Go-module dependency from webhookd on the operator's API package. Any
  breaking change to the operator's Go types is a build break for webhookd.
  Mitigated by: the operator's API package is versioned (`v1alpha1`) and CRD
  changes already require a version bump.
- Heavier dependency tree — controller-runtime pulls in klog, the controller
  manager packages, etc. Most of those are not used in webhookd (we never
  construct a `Manager`), but they're in `go.sum`.
- Does not compose cleanly with a future world where the Wiz operator becomes a
  third-party product we integrate with rather than one we own — typed clients
  require their Go module on import.

### Neutral

- If the operator ever becomes external, migrating webhookd to the dynamic
  client is a mechanical refactor contained to `internal/webhook/executor.go`.
  Not a blocker, not a concern today.

## Alternatives Considered

- **Raw `client-go` typed client.** Would work; we'd reimplement the SSA patch
  construction and the watch-until-condition logic. Rejected because
  controller-runtime already does both and is what the operator uses.
- **`client-go` dynamic client with `unstructured.Unstructured`.** Would
  eliminate the operator API module dependency. Rejected because field typos
  become runtime admission errors, and we already couple to the CRD shape —
  pretending otherwise gains nothing.
- **Generated clientset (via `code-generator`).** Another typed option, but it's
  the pattern from older operator codebases. Kubebuilder has converged on
  controller-runtime; there's no reason to fork.

## References

- DESIGN-0002 §K8s Client Choice.
- ADR-0005 — Server-Side Apply for custom resource reconciliation.
- controller-runtime client:
  <https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/client>
- Kubebuilder book: <https://book.kubebuilder.io/>
