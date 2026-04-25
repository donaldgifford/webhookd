---
id: ADR-0006
title: "Synchronous response contract for webhook providers"
status: Accepted
author: Donald Gifford
created: 2026-04-24
---

<!-- markdownlint-disable-file MD025 MD041 -->

# 0006. Synchronous response contract for webhook providers

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

Webhook-driven provisioning has two possible shapes:

1. **Fire-and-forget.** Return 202 immediately on receipt, enqueue the work,
   process it on a background worker, and notify the provider of the final
   outcome out-of-band (e.g. calling back via the provider's REST API).
2. **Synchronous.** Do the work on the request goroutine, block until the
   downstream system (here, the Wiz operator) reports success or terminal
   failure, and return that outcome in the HTTP response.

The fire-and-forget shape is appealing at first glance — it decouples the
ingress latency from the reconciliation latency, and it maps naturally to
"ingestion is one service, processing is another." But for JSM (Phase 2), it has
concrete downsides:

- JSM's ticket state machine advances based on webhook response codes. If we 202
  on receipt, the ticket moves to "Provisioning in Progress" and has no
  mechanism to correct itself if the CR later fails.
- Calling back via the JSM REST API requires API credentials, rate-limit
  handling, and its own retry plumbing — surface we do not need if JSM's webhook
  response is the acknowledgement.
- JSM already gives us retry for free on 5xx. Duplicating that with an internal
  queue adds coordination between replicas, a dead-letter store, and worker
  lifecycle management — all persistent state we otherwise don't have.
- At expected volume (tens of requests per minute), there's no throughput reason
  to decouple.

The apply-and-watch-and-respond pattern fits because:

- `SAMLMapping` reconciliation in the Wiz operator is expected to complete
  within seconds (well inside any reasonable webhook timeout).
- JSM retries on 5xx, and SSA makes every retry idempotent (ADR-0005).
- webhookd stays stateless — no queue, no worker pool, no persistence.

## Decision

The webhook handler path for every Phase 2 provider is synchronous by default:

1. Verify signature.
2. Parse payload → produce an `Action`.
3. Executor applies the action to Kubernetes.
4. Executor watches the resulting CR status until Ready / terminal failure /
   timeout (`WEBHOOK_CR_SYNC_TIMEOUT`, default 20s).
5. HTTP response carries the outcome and a trace ID.

Status codes map to provider retry semantics: 200 means advance, 4xx means
"don't retry, surface to a human," 5xx means "transient, retry is safe."

Async execution is explicitly not added until a concrete provider demonstrates a
need for it (e.g. a Slack events callback that only expects a 200 ack and
doesn't want a sync outcome). When that happens, it's a second code path
alongside the synchronous one — not a replacement.

## Consequences

### Positive

- webhookd remains stateless: no queue, no worker pool, no persistence. Crash
  recovery is re-delivery.
- JSM's retry mechanism IS our retry mechanism. One retry policy to reason
  about, tuned by the source of truth (JSM admin).
- Idempotency is guaranteed by SSA + deterministic CR naming; retries are safe
  without coordination.
- End-to-end tracing works naturally because the span is live across the whole
  apply-and-watch operation. No background-worker trace stitching.
- Response carries the outcome directly, so JSM automation rules can branch on
  both status code and body ("Unknown project: aws-sandbox") to surface reasons
  back to the ticket requester.

### Negative

- The HTTP request ties up a webhookd goroutine (and thus a potential
  concurrent-capacity slot) for the duration of reconciliation. At expected
  volume this is fine; at 1000x expected volume it would be a problem — we'd
  have to revisit.
- The `WEBHOOK_CR_SYNC_TIMEOUT` must stay below the provider's webhook timeout
  (for JSM, ≥30s typical). Any provider whose webhook timeout is shorter than
  our reconcile budget is not a good fit for the synchronous path.
- A provider's webhook delivery retries may re-trigger reconciliation before the
  previous one finishes (both replicas watching, both eventually returning
  success). JSM handles duplicate successes cleanly; another provider might not.
  Revisit per-provider.

### Neutral

- The synchronous contract is enforced in the dispatcher/executor split, not in
  the provider. A provider's `Handle` method is pure and produces an `Action`;
  the dispatcher decides whether to execute synchronously or asynchronously. An
  async path in the future is a second dispatcher branch, not a rewrite of
  providers.

## Alternatives Considered

- **Fire-and-forget with JSM REST callback.** Decouple response latency from
  reconcile latency; post the final outcome as a ticket comment or status
  transition via JSM's REST API. Rejected for Phase 2: adds JSM API credentials,
  rate-limit handling, and an independent retry system for a problem JSM already
  solves with webhook retries.
- **Accept + enqueue + dedicated worker pool.** Internal queue with persistence.
  Rejected: introduces the first persistent state in the service, duplicates
  JSM's retry, and adds coordination between replicas. Not justified at
  projected volume.
- **Accept + process in goroutine, no response correlation.** The worst of both
  worlds — loses the synchronous contract without gaining decoupling. Not
  considered seriously.

## References

- DESIGN-0002 §Overview, §HTTP Response Contract, §Idempotency and Concurrency.
- ADR-0005 — Server-Side Apply for custom resource reconciliation (the other
  half of why synchronous is safe).
- JSM automation webhooks:
  <https://support.atlassian.com/cloud-automation/docs/jira-automation-triggers/>
