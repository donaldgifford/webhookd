---
id: ADR-0008
title: "Provider-callback pattern over durable queues for long-running work"
status: Proposed
author: Donald Gifford
created: 2026-04-30
---
<!-- markdownlint-disable-file MD025 MD041 -->

# 0008. Provider-callback pattern over durable queues for long-running work

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

ADR-0006 chose a synchronous response contract: webhookd applies the work and returns the outcome in the original HTTP response. That contract is load-bearing for the existing JSM workflow because JSM's automation rule reads the response body to advance the ticket — there is no callback hook for "the result will arrive later."

Synchronous works while every backend completes within an HTTP-timeout budget. The current K8s SSA-and-watch backend does (single-digit seconds). But the project's roadmap explicitly anticipates backends whose work won't fit:

- AWS Step Functions or multi-stage workflow backends (minutes).
- Future GitHub Backend that must wait on long-running CI before posting a PR comment.
- Anything where downstream rate limits force batching.

Two architectural shapes can absorb work that doesn't fit a sync request:

- **Durable queue.** Dispatcher writes the request to a persistent store (NATS JetStream, Redis Streams, Postgres outbox), worker reads and processes asynchronously. Pod restart survives because state is durable; work resumes from the queue.
- **Provider callbacks.** Dispatcher returns `202 Accepted` immediately; backend runs in a goroutine; on completion, webhookd POSTs the result to the originator's callback URL. Pod restart loses the in-flight goroutine, but the originator's existing webhook-retry mechanism re-delivers the original signed request, and per-provider idempotency keys prevent duplicate work.

The investigation in INV-0001 (and the user's reframing during review) found that for the providers on the roadmap (JSM, GitHub, Slack), the callback pattern is genuinely sufficient — every one of them supports inbound webhooks for status updates, and most provide native callback idempotency (GitHub Checks API on `external_id`, Slack `chat.postMessage` on `client_msg_id`, JSM ticket comments are tolerantly idempotent). Adding a durable queue is a substantial operational commitment (NATS / Redis / Postgres deployment, monitoring, backup, queue-lag dashboards, dead-letter handling) for a problem the originating provider has already solved.

## Decision

For long-running backend work that exceeds an HTTP-timeout budget, webhookd uses the **provider-callback pattern**, not a durable queue:

1. Dispatcher receives the webhook, verifies the signature, hits the per-provider idempotency check, and decides if the work fits in sync.
2. If it does (current K8s case), respond synchronously per ADR-0006.
3. If it doesn't, the matching `AsyncBackend` returns a `PendingToken`; the dispatcher responds `202 Accepted` with that token in the body.
4. webhookd runs the work in a goroutine. When it completes, the goroutine invokes the Provider's `Callback` helper, which POSTs the final result to the originator's callback URL with an HMAC-signed body.
5. Pod-crash recovery is provided by the originating provider's webhook-retry behavior + per-provider idempotency keys (the second attempt is detected as a duplicate). Callback double-fires are absorbed by provider-side idempotency.

Backends opt into async by implementing the `AsyncBackend` interface alongside (or instead of) the synchronous `Backend` interface; sync remains the default.

Durable queue infrastructure (NATS JetStream, Postgres outbox, etc.) is deferred to a future RFC, scoped to a specific failure mode the callback pattern can't cover. The four documented failure modes are:

- Originating provider doesn't support inbound webhook callbacks.
- Fan-out to N backends with independent retry semantics.
- Compliance requires a durable per-event audit trail.
- Pod-crash callback dedupe complexity outgrows what `webhookd.io/callback-fired-at` annotations can carry.

None of these apply today. When one does, that's the trigger to revisit.

## Consequences

### Positive

- **Zero new infrastructure.** No NATS, no Redis, no Postgres outbox. The "queue" is the originating provider's own webhook-retry mechanism.
- **Provider-native.** JSM, GitHub, Slack, Linear, PagerDuty, et al. all support inbound webhook callbacks for status updates. Reusing that path means no new contract.
- **Operationally honest.** Failure modes are observable in metrics + logs, not buried in queue-lag dashboards. Pod restarts recover via originator re-delivery.
- **Webhookd stays stateless.** No durable per-request state; the only persistence is the target system's own state (e.g., a K8s CR). Aligns with ADR-0006's stateless guarantee.
- **Sync stays the default.** Backends that can complete in time keep the simpler contract; async is opt-in per backend.
- **Migration path is clear.** If any of the four failure modes shows up later, swapping in a durable queue is a localized change behind the same `Backend` / `AsyncBackend` interfaces.

### Negative

- **Pod crash mid-callback can leave the originator hanging.** If a pod crashes after the work succeeds but before the callback POST fires, the originator gets no signal. Mitigations are layered:
  1. Trust provider-side callback idempotency (most providers tolerate duplicate updates).
  2. Stamp `webhookd.io/callback-fired-at` annotation on the target K8s resource after a successful callback POST; retried webhooks read it and skip re-firing.
  3. (Phase 5, deferred) durable queue.
- **Per-provider implementation cost.** Each Provider that supports async needs a small `Callback(token, result)` helper that POSTs to the originator's URL. Not free, but small (~50 LOC per provider).
- **Outbound callbacks must be HMAC-signed.** webhookd becomes a signature-emitting client, not just a verifier. Adds a small amount of code; standard pattern.
- **No durable audit trail of webhook events.** Logs are the only record. If compliance ever requires durable per-event audit, this assumption breaks and the durable-queue path becomes mandatory.

### Neutral

- The async path is a second branch in the dispatcher (`Backend.Execute` synchronous vs `AsyncBackend.ExecuteAsync` + `AsyncBackend.Callback`), not a separate worker process. Same binary, same observability, same deployment.
- Adding a durable queue later is non-breaking for backends that already implement `Backend` / `AsyncBackend` — the queue would sit in front of the dispatcher, not replace it.

## Alternatives Considered

- **NATS JetStream as the durable backbone.** Strong fit for a future fan-out use case (one event → N backends with independent retries). Adds a 3-replica StatefulSet, ~500 MB-1 GB memory, persistent volumes, and ops surface (monitoring, backup). Deferred to Phase 5; revisit when an actual fan-out workload shows up. Argo Events (rejected as the platform in INV-0002) uses NATS JetStream as its EventBus and is a candidate Backend at that point.
- **Postgres outbox.** Cheaper if we already run Postgres elsewhere — write the request to a table, worker polls/listens. Doubles as a compliance audit store. Currently no shared Postgres in the deployment; deferred until either a queue need or a compliance need shows up.
- **Redis Streams.** Cheaper than NATS, but its primary use case is caching, not durable messaging. Workable but feels grafted on. Runner-up to NATS JetStream if durable queues become necessary.
- **External managed queue (SQS / Pub/Sub / Service Bus).** Zero ops, but locks the deployment to a cloud vendor. Fine option if and when we need it — the abstraction at the dispatcher level doesn't care which durable queue is behind it.
- **Argo Events as the platform.** Rejected for the JSM workflow specifically because the EventBus → Sensor → Trigger pipeline is async-by-design and breaks the synchronous response contract. See INV-0002 for the full evaluation. Argo Events remains a candidate Backend integration for future fan-out workloads — that's the role this ADR explicitly sets up for it.

## References

- RFC-0001 — Generalize webhookd to Provider × Backend with Multi-Tenant Routing (the parent proposal that surfaces this decision).
- INV-0001 — Multi-Provider Multi-Backend Architecture Review §State management.
- INV-0002 — Evaluate Argo Events as Alternative for JSM-to-CR Webhook Workflow.
- ADR-0006 — Synchronous response contract for webhook providers (the foundation; this ADR extends it cleanly for cases the sync contract can't cover).
- JSM automation webhooks (the canonical example of a provider that supports both inbound webhooks and inbound callbacks via REST API).
