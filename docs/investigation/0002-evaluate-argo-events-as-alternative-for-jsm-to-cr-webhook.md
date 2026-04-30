---
id: INV-0002
title: "Evaluate Argo Events as Alternative for JSM-to-CR Webhook Workflow"
status: Open
author: Donald Gifford
created: 2026-04-30
---
<!-- markdownlint-disable-file MD025 MD041 -->

# INV 0002: Evaluate Argo Events as Alternative for JSM-to-CR Webhook Workflow

**Status:** Open
**Author:** Donald Gifford
**Date:** 2026-04-30

<!--toc:start-->
- [Question](#question)
- [Hypothesis](#hypothesis)
- [Context](#context)
- [Approach](#approach)
- [Findings](#findings)
  - [How Argo Events models a webhook flow](#how-argo-events-models-a-webhook-flow)
  - [The synchronous-response gap](#the-synchronous-response-gap)
  - [Workarounds for async, and what they cost JSM](#workarounds-for-async-and-what-they-cost-jsm)
  - [What Argo Events does well that webhookd would have to build](#what-argo-events-does-well-that-webhookd-would-have-to-build)
  - [Operational footprint](#operational-footprint)
  - [Side-by-side fit table](#side-by-side-fit-table)
- [Conclusion](#conclusion)
- [Recommendation](#recommendation)
- [Open Questions for Review](#open-questions-for-review)
- [References](#references)
<!--toc:end-->

## Question

Can the existing JSM webhook → SAMLGroupMapping CR → return success/fail to JSM workflow be satisfied by Argo Events alone — *without* shipping webhookd — including the synchronous response contract JSM's automation rule depends on (ADR-0006)? If yes, what does the manifest set look like and what infrastructure does it require? If no, what specifically is blocking and how invasive is a workaround?

## Hypothesis

Argo Events covers ~80% of the workflow elegantly (webhook ingestion, payload filtering, CR creation, retry semantics, replay) but fails on the **synchronous response** requirement — the webhook EventSource is async by design: it acknowledges receipt with 200 OK and fires the trigger out-of-band. JSM's automation rule reads the HTTP response body to decide what to do next; with Argo Events as the receiver, that response is "received, will process," not "applied and Ready." Closing the gap requires either (a) reworking the JSM-side automation to be async-tolerant (poll status, separate callback flow) or (b) inserting a custom proxy in front of Argo Events that does the synchronous wait — which is most of what webhookd is, defeating the purpose.

## Context

**Triggered by:** INV-0001 follow-up. The user already runs ArgoCD and is open to Argo Rollouts, so adding Argo Events is *not* a meaningful operational-cost concern. If Argo Events can satisfy the existing JSM workflow without webhookd, that changes the strategic value of building out webhookd's multi-Provider/multi-Backend story (INV-0001).

The existing workflow (DESIGN-0002 + IMPL-0002):

1. JSM automation rule POSTs to `https://webhookd.example/webhook/jsm` with HMAC-signed body containing the issue's status + custom fields.
2. webhookd verifies the signature, decodes the payload, and **only if** the status matches the configured trigger, builds a `SAMLGroupMapping` spec from the custom fields.
3. webhookd applies the CR via Server-Side Apply.
4. webhookd opens a Watch on the CR, blocks until `Ready=True` *or* the configured `WEBHOOK_CR_SYNC_TIMEOUT` elapses.
5. webhookd returns one of:
   - `200 success` (Ready=True observed) — JSM marks the ticket Approved+done.
   - `200 noop` (status didn't match trigger) — JSM advances the ticket without retry.
   - `504 timeout` — JSM retries (or pages a human, depending on the rule).
   - `400/422` — JSM marks ticket as failed with the reason.

The synchronous result is **load-bearing**: ADR-0006 explicitly chose this contract because JSM's automation rule has no callback hook — the only thing it can react to is the HTTP response from the original webhook POST.

## Approach

1. Map each step of the existing flow onto an Argo Events shape (EventBus → EventSource → Sensor → Trigger) and identify gaps.
2. Locate the synchronous-response question precisely — what *can* be returned in the EventSource's HTTP response, and what *cannot*?
3. Enumerate workarounds for the async gap and weigh them against ADR-0006.
4. Compare what Argo Events ships out-of-the-box vs. what webhookd has built (HMAC signing, structured logging with trace correlation, rate limiting per provider, replay protection, response shaping).
5. Inventory the runtime infrastructure cost (controllers, EventBus / NATS JetStream, RBAC).
6. Assess whether running both — Argo Events for some flows, webhookd for the synchronous JSM flow — is coherent or fractured.

## Findings

### How Argo Events models a webhook flow

The pieces map roughly like this:

| Argo Events resource | What it does | Maps to webhookd's… |
|---|---|---|
| `EventBus` | Durable message bus (NATS JetStream is the default; Kafka is also supported). One per namespace; shared by all EventSources / Sensors in that namespace. | (no equivalent — webhookd is in-process, no bus) |
| `EventSource` | Receives external events. The `webhook` source-type runs an HTTPS listener, validates against optional auth (basic auth / OAuth-bearer / mutual TLS), and publishes the event payload to the EventBus. Other source types: SQS, Kafka, GitHub, GitLab, Slack, S3, calendar, file… | The dispatcher's HTTP entry + signature verification |
| `Sensor` | Watches the EventBus, evaluates dependencies (boolean expressions over event payloads using `data.<jsonpath>`-style filters), and fires one or more triggers when the dependency conditions are met. | The Provider's `Handle` step (filtering + decoding) |
| `Trigger` (within a Sensor) | The actual action: create/patch a K8s resource (custom or built-in), execute an Argo Workflow, POST to an HTTP endpoint, send a Slack message, … Trigger templates support payload-driven field substitution via `parameters`. | The executor's `apply` step |

A first-pass implementation of the JSM flow would be:

```yaml
# 1. EventBus — once per namespace
apiVersion: argoproj.io/v1alpha1
kind: EventBus
metadata: { name: default, namespace: argo-events }
spec:
  jetstream:
    version: latest
    replicas: 3

---
# 2. EventSource — the HTTPS listener JSM POSTs to
apiVersion: argoproj.io/v1alpha1
kind: EventSource
metadata: { name: jsm, namespace: argo-events }
spec:
  service:
    ports: [{ port: 12000, targetPort: 12000 }]
  webhook:
    jsm-tenant-a:
      port: "12000"
      endpoint: /jsm/tenant-a
      method: POST
      # NOTE: Argo Events webhook EventSource supports basic auth and
      # mTLS, but does NOT natively support HMAC-SHA256 signature
      # verification. See gap below.

---
# 3. Sensor — filter for trigger status, fire CR-create trigger
apiVersion: argoproj.io/v1alpha1
kind: Sensor
metadata: { name: jsm-samlgroupmapping, namespace: argo-events }
spec:
  dependencies:
    - name: jsm-approved
      eventSourceName: jsm
      eventName: jsm-tenant-a
      filters:
        data:
          - path: body.issue.fields.status.name
            type: string
            value: ["Approved"]
  triggers:
    - template:
        name: create-samlgroupmapping
        k8s:
          operation: create  # or patch for SSA
          source:
            resource:
              apiVersion: wiz.webhookd.io/v1alpha1
              kind: SAMLGroupMapping
              metadata:
                name: ""  # filled in via parameters
                namespace: wiz-operator
              spec: {}    # filled in via parameters
          parameters:
            - src:
                dependencyName: jsm-approved
                dataKey: body.issue.key
              dest: metadata.name  # transformed via expr to lowercase + prefix
            - src:
                dependencyName: jsm-approved
                dataKey: body.issue.fields.customfield_10001
              dest: spec.providerGroupId
            # ... role, project, identityProviderId
```

This *is* a working JSM-event → CR-creation pipeline. But it ends at "CR created." Read on.

### The synchronous-response gap

This is the central finding — and the one I'd validate before relying on it, because async-vs-sync is the kind of detail that occasionally has an edge in product docs the way I remember it not having one.

**As of the last time I checked the Argo Events webhook EventSource:**

- It returns `200 OK` with a body of `success` (or similar fixed string) **as soon as it has published the event to the EventBus**, *before* the Sensor fires.
- There is no built-in mechanism for the HTTP response body to reflect the trigger's outcome.
- The Sensor and the EventSource are decoupled by design — that's the entire point of the EventBus. The webhook caller cannot know which Sensor consumed its event, much less wait for that Sensor's trigger to complete.

**What that means for the JSM workflow:**

- ✅ Step 1 (receive POST + auth): Argo Events handles via the EventSource (with caveats; see HMAC gap below).
- ✅ Step 2 (filter on status): Argo Events handles cleanly via Sensor `dependencies.filters.data`.
- ✅ Step 3 (apply CR): Argo Events handles via the K8s trigger.
- ❌ Step 4 (watch the CR until Ready=True): no equivalent. The Sensor fires the trigger and is done.
- ❌ Step 5 (return Ready/timeout/failed to JSM): the response is 200 OK regardless of the trigger's eventual outcome.

So Argo Events satisfies the **delivery** of the workflow but not the **synchronous response contract** that JSM's automation rule depends on.

### Workarounds for async, and what they cost JSM

If we're willing to break ADR-0006, four shapes are possible. None are free.

**Workaround A — JSM-side polling.** JSM automation rule POSTs to Argo Events, gets 200, then polls a separate `GET /status?issue=SEC-1234` endpoint until status resolves.

- Cost: that status endpoint is a *new service* — Argo Events doesn't have one. We'd build it (querying the CR's status).
- Cost: JSM automation-rule logic gets significantly more complex; "wait for HTTP response" is one block, "poll until done" is several.
- Verdict: defeats the purpose. We've replaced webhookd's synchronous wait with a more complicated client-side wait *and* still need a service to query CR status.

**Workaround B — Two-Sensor callback.** First Sensor creates the CR; a second Sensor watches CRs for `Ready=True`/`Ready=False` and fires an HTTP trigger that POSTs the result back to a JSM REST endpoint (e.g., a ticket comment or status transition).

- Cost: requires JSM-side acceptance of unsolicited callbacks. JSM Cloud automation can do this via REST API calls *into* Jira, but not via "wait for arbitrary external service to call back into a paused automation rule." That's not how JSM automation works.
- Cost: two Sensors per integration, one of which watches CRs (re-implementing parts of the controller-runtime watch loop in trigger config).
- Verdict: works *if* we can rewrite the JSM-side automation to use a comment-driven workflow, but that's a major scope change for the human operators using JSM.

**Workaround C — Custom proxy in front of Argo Events.** A small service that takes the JSM webhook synchronously, publishes to Argo Events' EventBus, then watches the CR (or watches the Sensor's outcome) and returns the result.

- Cost: this is *most of what webhookd is*. We'd be building webhookd-the-proxy on top of Argo Events instead of webhookd-the-receiver standalone, with worse isolation (now there are two systems to operate, and the contract between them lives in EventBus topics).
- Verdict: defeats the purpose. If we're building the synchronous proxy anyway, just keep webhookd.

**Workaround D — Argo Workflows trigger with `synchronization` + workflow-status response.** Have the Sensor fire an Argo Workflow that does the apply + watch, and use the Workflow's webhook-style synchronous waiter. (Argo Workflows has experimental "wait for workflow completion" patterns via the WebsocketServer or via Workflow-of-Workflows.)

- Cost: this is a workflow engine for what's intrinsically a one-step operation. Massive complexity bump.
- Cost: still needs a synchronous front to hide the workflow state machine from JSM.
- Verdict: wrong tool. Argo Workflows is for multi-step pipelines, not "do one CR apply and respond."

**The pattern across all four:** if you keep ADR-0006 (sync response, JSM gets the result in the original POST), Argo Events alone is insufficient. If you drop ADR-0006, the cheapest alternative (A or B) requires non-trivial JSM-side work *plus* still-new infrastructure (a status endpoint or callback handler).

### What Argo Events does well that webhookd would have to build

Setting the sync gap aside, Argo Events ships a number of features webhookd doesn't yet have:

| Feature | Argo Events | webhookd today | Cost to add to webhookd |
|---|---|---|---|
| Persistent event bus / replay | ✅ NATS JetStream | ❌ in-process only | Significant — see INV-0001 §State management |
| At-least-once delivery to triggers | ✅ via JetStream durables | ❌ N/A (sync) | Tied to the above |
| Multi-EventSource fan-in (Slack + JSM + GH all into one Sensor) | ✅ | ❌ one Provider per request | Maps onto INV-0001's multi-Provider direction |
| Triggers beyond K8s (HTTP, Slack, Argo Workflows, Lambda, OpenWhisk, Pulsar, NATS, AWS Lambda, GCP Pub/Sub, Azure Event Hubs, Email, …) | ✅ | ❌ K8s-only | INV-0001's multi-Backend direction |
| Built-in retry / circuit-break on triggers | ✅ | ❌ | Modest |
| Declarative payload filters via `data.path == value` | ✅ | (in Provider code, per-provider) | (already done per-provider) |
| Helm chart for the controllers | ✅ | (we're standing this up for ourselves) | N/A — this is a wash |
| HMAC-SHA256 signature verification on webhook | ⚠️ Not built-in for the generic `webhook` source — only specific named EventSources (GitHub, GitLab, Bitbucket, Slack) verify their vendor-specific signatures | ✅ first-class | (already done) |
| OpenTelemetry traces / Prometheus metrics with consistent labels | ⚠️ Partial — controller emits metrics, but per-event tracing isn't first-class | ✅ first-class | (already done) |

The HMAC gap is worth highlighting — JSM signs requests with HMAC-SHA256 against a custom header. Argo Events' generic `webhook` source supports basic auth and OAuth bearer, not HMAC. Closing this gap means either (a) writing a custom EventSource (Go code, controller-runtime style — same engineering as building webhookd's signature path), or (b) fronting Argo Events with an HMAC-verifying proxy, which is again most of webhookd.

### Operational footprint

If we adopted Argo Events for this flow, the runtime additions are:

- **`argo-events-controller-manager`** — one Deployment, ~150 MB. Watches EventSource / Sensor / EventBus CRs.
- **`eventsource-controller`** — managed pods per EventSource. The webhook EventSource runs as its own Deployment that the controller spins up.
- **`sensor-controller`** — managed pods per Sensor.
- **NATS JetStream** — required as the EventBus. Argo Events ships an embedded JetStream-StatefulSet via the EventBus CR. Three replicas recommended for HA. Adds ~500 MB-1 GB memory + persistent volume claim for the JetStream stream.
- **Five new CRDs**: `EventBus`, `EventSource`, `Sensor`, plus two operator-internal ones.
- **RBAC**: the Sensor's K8s trigger needs `create` (or `patch` for SSA) on `samlgroupmappings` in the target namespace — same shape as webhookd's existing Role.

You already run ArgoCD, so the operational learning curve is partial — same project, same install patterns, same cluster permissions model. But it is *more* infrastructure than webhookd-standalone.

### Side-by-side fit table

| Requirement (current JSM flow) | Argo Events alone | webhookd | Argo Events + custom proxy |
|---|---|---|---|
| Receive HTTPS POST from JSM | ✅ | ✅ | ✅ |
| HMAC-SHA256 signature verification | ❌ (build custom EventSource) | ✅ | ✅ (proxy does it) |
| Replay protection (timestamp window) | ❌ (build custom EventSource) | ✅ | ✅ (proxy) |
| Filter on issue status, dispatch only on match | ✅ (Sensor filters) | ✅ | ✅ (or Sensor) |
| Apply SAMLGroupMapping via SSA | ✅ (K8s trigger w/ patch) | ✅ | ✅ |
| Watch CR until Ready=True or timeout | ❌ | ✅ | ✅ (proxy) |
| Return success/noop/failure to JSM **synchronously** | ❌ | ✅ | ✅ (proxy) |
| Trace context propagation onto CR | ❌ | ✅ (ADR-0007) | ✅ (proxy) |
| Provider-specific response body for JSM ticket comments | ❌ | ✅ | ✅ (proxy) |
| Per-provider rate limiting | ❌ (cluster-wide ingress only) | ✅ | ✅ (proxy) |
| Multi-tenant routing (`/{provider}/{id}`) | ✅ (one EventSource per webhook) | ⚠️ Phase-0 today; INV-0001 direction | ✅ |

## Conclusion

**Answer: No** — Argo Events alone cannot satisfy the existing JSM-to-CR workflow. It can ingest the webhook, parse it, and apply the CR; it cannot return the *result* (Ready=True / timeout / failure) to JSM in the original HTTP response, because the EventSource → EventBus → Sensor → Trigger pipeline is async by design.

Closing the gap requires either:

1. Breaking ADR-0006 and reworking JSM-side automation to be async-tolerant (workarounds A or B). This is a **non-trivial JSM-side engineering effort** and changes the user-visible behavior (tickets get marked "received" then "applied" via a separate transition).
2. Inserting a custom proxy that does the synchronous wait (workaround C). This is essentially **rebuilding webhookd**, just with Argo Events as a backend instead of `client.WithWatch`. Worse isolation, more moving parts, no real win.

**Argo Events is a good fit when:** the workflow is fire-and-forget (CI triggers, S3 events, scheduled jobs) and the originating system is OK with 200-OK-as-acknowledgement. It would be a great fit for a *future* webhookd Backend that needs async fan-out (e.g., "JSM ticket Approved → also notify Slack + create GitHub issue + apply CR" — three triggers from one event). But it's not a fit for the synchronous JSM contract that exists today.

**Important caveat on this conclusion:** my claim that the webhook EventSource has no synchronous-trigger-result mode is based on the project's design principles (decoupling via EventBus is *the point*) and the documentation as I know it. Before locking this in, I'd:

- Spin up Argo Events on a kind cluster with a webhook EventSource + Sensor + a no-op trigger and verify the response timing empirically (does the 200 land before the trigger executes? — I'm 95% certain yes, but it's a 30-minute experiment).
- Skim the Argo Events 1.9+ changelog for any "synchronous trigger result" feature (unlikely but worth checking — the project does add features).
- Check if any external proxy (e.g., the `webhook-event-source` mode with `customResponse: { onSuccess: ..., onFailure: ... }`) covers a fraction of this — I don't recall such a feature but the recent versions add things.

If those checks turn up a synchronous mode I missed, this conclusion would flip from "no" to "yes, with caveats."

## Recommendation

1. **Continue building webhookd** for the synchronous JSM workflow as currently designed. Argo Events is not a substitute here.
2. **Pin Argo Events as a known good fit for INV-0001's async-backend direction.** When webhookd grows backends that don't need synchronous results (e.g., "fan out to N notification channels"), Argo Events becomes a candidate Backend implementation rather than a competing platform.
3. **Run the 30-minute kind experiment** to empirically validate the sync-response gap before this conclusion gets cited downstream. Note as a follow-up task on this INV.
4. **Don't run both side-by-side in production** for the JSM flow — that's two systems with overlapping responsibilities and unclear ownership of "what happens if the apply succeeds in webhookd but the Sensor sees the same event from a webhook test fire?" Pick one per flow.

## Open Questions for Review

> **Status:** Deferred — flagged for revisit, not blocking. This investigation is intentionally in progress; we don't have firm answers yet and don't want to force them prematurely. Each question below is parked until either the empirical validation runs, INV-0001 progresses, or external context changes (JSM automation capabilities, Argo Events release notes).

1. **(Deferred) Is the synchronous-response contract negotiable on the JSM side?** If JSM automation can be redesigned to handle async (status comments, two-stage transitions), workarounds A or B become viable and Argo Events covers ~95% of the remaining flow. Revisit when there's product appetite to change the JSM-side rule.
2. **(Deferred) Is there appetite for an empirical validation?** I'm 95% confident on the sync gap but haven't actually re-tested it on a current Argo Events release. A 30-min kind experiment would lock the conclusion in or surface a feature I missed. Revisit before this INV gets cited downstream as definitive.
3. **(Deferred) For future async backends (INV-0001 Phase 4):** is "Argo Events as a Backend in webhookd" coherent, or is it cleaner to pick exactly one of {webhookd-with-its-own-bus, Argo Events as the platform, both}? Current lean is webhookd-with-its-own-bus for synchronous flows + Argo Events for async fan-out, but it's a Phase-4 call. Revisit when the first async backend lands.
4. **(Deferred) NATS JetStream redundancy:** if INV-0001's async direction picks NATS JetStream and we *also* run Argo Events (which uses NATS JetStream as its EventBus), can we share one JetStream cluster, or do they need to be isolated for blast-radius reasons? Revisit alongside Q3.

## References

- INV-0001 — Multi-Provider Multi-Backend Architecture Review (the parent investigation)
- ADR-0006 — Synchronous response contract (the constraint that disqualifies Argo Events here)
- DESIGN-0002 — JSM webhook → SAMLGroupMapping CR provisioning (the workflow under evaluation)
- IMPL-0002 §Resolved Decisions — current Provider/executor contract
- [Argo Events project](https://argoproj.github.io/argo-events/)
- [Argo Events EventSource types (incl. webhook)](https://argoproj.github.io/argo-events/eventsources/setup/webhook/)
- [Argo Events Sensor + Trigger model](https://argoproj.github.io/argo-events/sensors/triggers/k8s-object-trigger/)
- [Argo Events EventBus (NATS JetStream)](https://argoproj.github.io/argo-events/eventbus/jetstream/)
