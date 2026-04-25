---
id: ADR-0005
title: "Server-Side Apply for custom resource reconciliation"
status: Accepted
author: Donald Gifford
created: 2026-04-24
---
<!-- markdownlint-disable-file MD025 MD041 -->

# 0005. Server-Side Apply for custom resource reconciliation

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

When webhookd receives a JSM status-transition webhook (Phase 2), it has to
translate the ticket contents into a `SAMLMapping` custom resource and get
that CR into the cluster. JSM retries on 5xx. The same logical event can
arrive at webhookd multiple times — from JSM retry, from a duplicate
automation rule firing, from multiple webhookd replicas each seeing the
delivery. Two additional wrinkles:

- A ticket can be edited and re-transitioned, which means the spec we apply
  changes between deliveries.
- A human might `kubectl edit` a CR for an ad-hoc fix; we need to decide
  whether webhookd's apply stomps over that edit.

The three real options for getting an object into the cluster are:

1. `Get` → `Update` (read-modify-write with `resourceVersion` conflict
   handling).
2. `Create`, falling back to `Patch` on "already exists."
3. **Server-Side Apply** (`Patch` with `client.Apply`), where each
   field-owning client declares the fields it manages and the API server
   merges changes.

Read-modify-write requires retry-on-conflict loops and the code has to
reason about `resourceVersion`. Create-or-patch duplicates logic and
doesn't help with partial-field ownership. SSA was built for this exact
pattern — multiple actors asserting desired state over the same object —
and Kubernetes uses it internally for `kubectl apply --server-side`.

## Decision

Use Server-Side Apply via
`client.Patch(ctx, obj, client.Apply, client.FieldOwner("webhookd"), client.ForceOwnership)`
for all CR writes from webhookd.

The field manager identity is configurable (`WEBHOOK_CR_FIELD_MANAGER`,
default `"webhookd"`). `ForceOwnership` is set: if another manager
previously touched a field webhookd wants to own, webhookd takes
ownership back.

A deterministic CR name keyed on the source identifier (e.g.
`jsm-<issue-key-lower>`) is required so retries converge on the same
object.

## Consequences

### Positive

- Idempotency is a property of the API call, not of our code. Same spec
  re-applied is a no-op: no generation bump, operator does not reconcile
  again. This is the JSM-retry case, handled automatically.
- Different spec for the same name (ticket edit) bumps the generation
  and triggers a fresh reconcile. This is the edit-and-resubmit case,
  also handled automatically.
- No retry-on-conflict loops; no `resourceVersion` bookkeeping.
- Multiple webhookd replicas racing on the same CR name don't need
  coordination. SSA merges deterministically.
- We get observable field ownership: `kubectl get ... -o yaml` shows
  `managedFields` with `manager: webhookd`, which is useful when
  debugging "who set this field?"

### Negative

- `ForceOwnership=true` means webhookd silently stamps over manual
  `kubectl edit` changes on fields webhookd owns. That is the right
  default for this service (the CR is meant to be webhookd-owned) but
  makes ad-hoc human fixes harder. Documented in the operator runbook.
- SSA requires the operator's API types to have correct `// +listType=map`
  and `// +listMapKey=...` markers on list fields to merge correctly
  instead of replacing whole slices. Any list-field changes in the CRD
  require a coordinated review with the operator team.
- Requires Kubernetes ≥1.22 for stable SSA (not a real constraint in
  2026).

### Neutral

- If webhookd ever needs to delete a CR (Phase 2.5 cleanup), SSA does
  not cover that; a separate `client.Delete` call is the right primitive.
  Out of scope here.

## Alternatives Considered

- **`Get` → construct → `Update` with retry-on-conflict.** Works, but
  every caller has to handle conflict loops and distinguish "CR doesn't
  exist yet, create it" from "CR exists, patch it." SSA collapses those
  into one code path.
- **`Create`; if AlreadyExists, `Update`.** Doesn't handle partial
  field ownership. Competing managers (webhookd + a human edit) will
  clobber each other without a principled merge.
- **JSON Patch / Strategic Merge Patch.** Works for small targeted
  updates but requires hand-building the patch document and doesn't
  express "this object is what I want to exist" — which is what we
  actually mean.

## References

- DESIGN-0002 §Applying the CR — Server-Side Apply.
- ADR-0004 — controller-runtime typed client.
- Kubernetes SSA reference:
  <https://kubernetes.io/docs/reference/using-api/server-side-apply/>
- controller-runtime `client.Apply`:
  <https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/client#Apply>
