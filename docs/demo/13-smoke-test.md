# 13. Smoke Test

End-to-end validation. We send a signed JSM payload, confirm a 200,
verify the CR was applied + reconciled, and inspect the metrics + traces.

This is the moment of truth — every architectural piece exercised in
one curl.

## Preconditions

```bash
just kind-up      # kind cluster + CRD installed
just dev-stack    # otel-collector + prometheus + jaeger via compose
```

You can run webhookd-demo two ways:

- **Native:** `just run` — fastest iteration loop
- **In-cluster:** `just bake && just kind-load && just deploy`

The smoke test below works for both.

## The send-jsm justfile recipe (preview)

```just
send-jsm webhook_id="demo-tenant-a":
    #!/usr/bin/env bash
    set -euo pipefail
    SECRET=topsecret
    PAYLOAD='{"issue":{"key":"ABC-123","fields":{"summary":"Platform team access to their Wiz project","status":{"name":"Approved"},"customfield_10001":"okta-platform-engineering","customfield_10002":"platform-engineer","customfield_10003":"platform-team"}}}'
    TIMESTAMP=$(date -u +"%Y-%m-%dT%H:%M:%SZ")
    SIG=$(printf '%s' "$PAYLOAD" | openssl dgst -sha256 -hmac "$SECRET" -hex | awk '{print $2}')
    curl -s -X POST "http://localhost:8080/jsm/{{webhook_id}}" \
      -H "Content-Type: application/json" \
      -H "X-Hub-Signature-256: sha256=$SIG" \
      -H "X-Webhook-Timestamp: $TIMESTAMP" \
      -d "$PAYLOAD" | jq
```

## Run it

```bash
just send-jsm
```

Expected response:

```json
{
  "status": "success",
  "trace_id": "9c43b2f1d6a8e0c4b7ab95f3e2d1c8a0",
  "request_id": "a3f5b2c1e4d68097"
}
```

## Verify the side effect

```bash
kubectl get samlgroupmappings -n wiz-operator
# NAME      READY   SYNCED   VALID   AGE
# abc-123   True                     3s

kubectl get samlgroupmapping -n wiz-operator abc-123 -o yaml
```

Should show:

```yaml
apiVersion: wiz.rtkwlf.io/v1alpha1
kind: SAMLGroupMapping
metadata:
  name: abc-123
  namespace: wiz-operator
  annotations:
    webhookd.io/jsm-issue-key: abc-123
    webhookd.io/trace-id: 9c43b2f1d6a8e0c4b7ab95f3e2d1c8a0    # ADR-0007
spec:
  identityProviderId: saml-idp-abc123
  providerGroupId: okta-platform-engineering
  description: "Platform team access to their Wiz project"
  roleRef:
    name: platform-engineer
  projectRefs:
  - name: platform-team
status:
  conditions:
  - type: Ready
    status: "True"
    lastTransitionTime: "2026-04-30T15:23:01Z"
    reason: MockOperator
    message: "demo: synthetically marked ready"
```

Three things to verify:

1. The CR exists in `wiz-operator` and matches the canonical
   [`samlmapping.example.yaml`](samlmapping.example.yaml) shape
2. The `webhookd.io/trace-id` annotation is present and matches the
   response body's `trace_id` (ADR-0007 propagation)
3. `Ready=True` is set (mock operator did its job)

## Verify metrics

Open <http://localhost:9091> (Prometheus). Try these queries:

```promql
# Request count, by instance and outcome.
webhookd_dispatch_total{}

# Backend sync duration, p95 over the last minute.
histogram_quantile(0.95, rate(webhookd_backend_sync_duration_seconds_bucket[1m]))

# In-flight HTTP requests.
webhookd_http_in_flight_requests
```

You should see at least one `webhookd_dispatch_total{instance="demo-tenant-a",kind="success"}`
counter ticked.

## Verify traces

Open <http://localhost:16686> (Jaeger). Service dropdown → `webhookd-demo`.
Find a trace with the `dispatcher.serve` root span.

Expected span tree:

```
dispatcher.serve                   POST /jsm/demo-tenant-a    8.4ms
└─ backend.execute                                            6.1ms
   ├─ (apply)                       webhookd-demo internals   1.2ms
   └─ (watch)                       waitForReady              4.8ms
```

Span attributes worth confirming:

- `webhook.provider_type=jsm`
- `webhook.instance_id=demo-tenant-a`
- `backend.type=k8s`
- `backend.request_kind=wiz.SAMLGroupMapping`
- `backend.result_kind=1` (ResultSuccess)
- `backend.http_status=200`

## Negative paths

Each of these should return a typed error response:

### Bad signature

```bash
curl -s -X POST http://localhost:8080/jsm/demo-tenant-a \
  -H "X-Hub-Signature-256: sha256=deadbeef" \
  -H "X-Webhook-Timestamp: $(date -u +"%Y-%m-%dT%H:%M:%SZ")" \
  -d '{"issue":{"key":"ABC-1","fields":{"status":{"name":"Approved"}}}}' | jq
```

```json
{
  "status": "error",
  "reason": "InvalidSignature",
  "detail": "invalid signature",
  "request_id": "..."
}
```

HTTP 401. `webhookd_signature_failures_total{provider="jsm"}` increments.

### Trigger status mismatch

Same payload as the happy path but flip `"status": {"name": "InProgress"}`:

```json
{
  "status": "noop",
  "reason": "TriggerStatusMismatch",
  "detail": "trigger status mismatch: got \"InProgress\", want \"Approved\"",
  "trace_id": "...",
  "request_id": "..."
}
```

HTTP **200** (intentional — Jira retries on 4xx, which we don't want
for benign mismatches).

### Idempotent retry

Re-fire the happy-path payload within 5 minutes:

```json
{
  "status": "noop",
  "reason": "DuplicateRequest",
  "detail": "idempotent retry",
  "trace_id": "...",
  "request_id": "..."
}
```

HTTP 200. `webhookd_idempotency_hits_total{instance="demo-tenant-a"}` ticks.

### Unknown webhook ID

```bash
curl -s -X POST http://localhost:8080/jsm/does-not-exist -d '{}' | jq
```

```json
{
  "status": "error",
  "reason": "UnknownInstance",
  "detail": "no instance configured for webhook_id",
  "request_id": "..."
}
```

HTTP 404.

### Backend timeout

Stop the mock operator (`Ctrl-C` on `just mock-operator`) and re-fire
the happy-path payload. The watch step waits the configured 20s, then:

```json
{
  "status": "timeout",
  "reason": "TimedOut",
  "detail": "waited 20s for Ready=True",
  "trace_id": "...",
  "request_id": "..."
}
```

HTTP 504.

## What we proved

- [x] HMAC verification works end-to-end
- [x] HCL config drives the routing decision
- [x] Provider parses → Backend executes → Provider shapes the response
- [x] OTel trace context propagates from request to CR annotation (ADR-0007)
- [x] Prometheus captures every code path with the right cardinality
- [x] Idempotency dedupes within the TTL window
- [x] Trigger-status-mismatch returns 200/no-op (Jira-retry-friendly)
- [x] Watch-step timeout returns 504 with a clean reason

The architecture works. Time to ship the real thing.

## What's still missing for production

The demo intentionally cuts corners. Before the real refactor lands:

- Replace the placeholder + Setup Backend pattern with explicit
  `webhook.NewRegistry()` registration in `main.go` (ADR-0010 §B
  alternative). The placeholder works but smells.
- Replace the dispatcher's string-match on Provider error messages
  (`respondHandleErr`) with a typed `ProviderError` interface
  (08-dispatcher.md §respondHandleErr (B/C alternatives)).
- Pre-allocated request ID + log + metric correlation through the
  whole pipeline — the demo passes traceID/requestID into BuildResponse
  but production wants them in every log line and metric label.
- Per-instance K8s clients when a `backend "k8s" { kubeconfig_env = "..." }`
  block names different kubeconfigs across instances (the demo uses
  the first-found `KUBECONFIG`).
- Goroutine leak detection (`go.uber.org/goleak.VerifyTestMain`).
- Lint + fuzz + envtest infrastructure.
- AsyncBackend support for the JSM workflow's slow-path (DESIGN-0004 §AsyncBackend).
- Hot reload (DESIGN-0004 §Migration §Hot reload).

Pull these forward as the production refactor's IMPL-0004 phases.
