# webhookd

A small Go service that receives signed webhooks, verifies them with HMAC-SHA256
plus replay-protection, and emits trace-correlated structured logs and Prometheus
metrics. The Phase 1 substrate handles routing, signature, observability, rate
limiting, and graceful shutdown. Phase 2 adds the JSM-to-Kubernetes
provisioning path: a JSM webhook lands at `POST /webhook/jsm`, gets verified
and decoded, the JSM provider builds a `SAMLGroupMapping` spec, the executor
SSA-applies the CR and watches its status until `Ready=True` or timeout.

**Status:** Phase 1 (DESIGN-0001 / IMPL-0001) shipped to `main` in
[PR #7](https://github.com/donaldgifford/webhookd/pull/7). Phase 2
(DESIGN-0002 / IMPL-0002) is implemented end-to-end on
`feat/design-0002-impl` — dispatcher, JSM provider, executor with envtest
coverage, observability metrics, and ADR-0007 for cross-process trace
propagation.

## Quick start

```bash
# Build the local image (linux/<your arch>):
docker buildx bake webhookd-local

# Run with the minimum required config:
docker run --rm \
  -p 8080:8080 -p 9090:9090 \
  -e WEBHOOK_SIGNING_SECRET=topsecret \
  -e WEBHOOK_TRACING_ENABLED=false \
  webhookd:dev

# In another shell:
curl -s :9090/healthz   # 200 ok
curl -s :9090/readyz    # 200 ready
curl -s :9090/metrics | head
```

To send a signed delivery, compute `sha256=<hex(hmac_sha256(secret, "v0:" + ts + ":" + body))>`:

```bash
SECRET=topsecret
BODY='{"event_type":"push","data":{}}'
TS=$(date +%s)
SIG="sha256=$(printf 'v0:%s:%s' "$TS" "$BODY" | openssl dgst -sha256 -hmac "$SECRET" -hex | awk '{print $2}')"

curl -i -X POST :8080/webhook/github \
  -H "X-Webhook-Signature: $SIG" \
  -H "X-Webhook-Timestamp: $TS" \
  -d "$BODY"
# HTTP/1.1 404 Not Found  — github is not in WEBHOOK_PROVIDERS by default.
# Use /webhook/jsm with a real JSM payload, or set WEBHOOK_PROVIDERS=""
# to land on the 503 tombstone (no provider wired).
```

## HTTP API

### Public listener (default `:8080`)

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/webhook/{provider}` | Receive a signed webhook. Returns `202` on success. |

The dispatcher verifies in this order: known provider → body size → HMAC
signature → provider decode → executor apply → watch sync. The first
failure determines the response status:

| Status | Cause |
|--------|-------|
| `200 OK` (`status:"success"`) | CR applied, operator marked `Ready=True` within `WEBHOOK_CR_SYNC_TIMEOUT`. |
| `200 OK` (`status:"noop"`) | Webhook understood but ticket was not in the configured trigger status — JSM advances without retry. |
| `400 Bad Request` | Malformed JSON, missing required JSM fields. |
| `401 Unauthorized` | Signature missing/invalid, timestamp missing/malformed/skewed. |
| `404 Not Found` | Path `/webhook/{provider}` references a provider not in `WEBHOOK_PROVIDERS`. |
| `413 Payload Too Large` | Body exceeded `WEBHOOK_MAX_BODY_BYTES`. |
| `422 Unprocessable Entity` | JSM custom field present but wrong type, or CRD schema rejected the spec at SSA. |
| `429 Too Many Requests` | Per-provider rate-limit exceeded. `Retry-After` header included. |
| `500 Internal Server Error` | Handler panicked, or RBAC misconfiguration (`IsForbidden` from the apiserver). |
| `503 Service Unavailable` | Transient K8s API error (`IsConflict`, `IsTooManyRequests`, watch disconnect). JSM should retry. |
| `504 Gateway Timeout` | CR applied but the operator did not mark `Ready=True` within `WEBHOOK_CR_SYNC_TIMEOUT`. JSM should retry. |

### Admin listener (default `:9090`)

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/healthz` | Always `200`. Liveness probe target. |
| `GET` | `/readyz` | `200` once startup wiring is done; flips to `503` on shutdown. |
| `GET` | `/metrics` | Prometheus exposition. |
| `GET` | `/debug/pprof/...` | Standard `net/http/pprof` endpoints. Gated by `WEBHOOK_PPROF_ENABLED`. |

### Signing format

The canonical message is `v0:<timestamp>:<body>` (Slack-style versioning so
the scheme can be revved later without breaking signers). Signers must:

1. Take the request body verbatim.
2. Take the Unix-second timestamp they will send in `X-Webhook-Timestamp`.
3. Concatenate `"v0:" + timestamp + ":" + body`.
4. Compute `hmac_sha256(secret, canonical)` and hex-encode.
5. Send `X-Webhook-Signature: sha256=<hex>` and `X-Webhook-Timestamp: <ts>`.

The receiver rejects any timestamp outside `[now − skew, now + skew]` to
defeat replays.

## Configuration

All configuration is via environment variables (see [ADR-0003](docs/adr/0003-environment-variable-only-configuration.md) for why). `WEBHOOK_SIGNING_SECRET` is the only required value; everything else has a sensible default.

| Variable | Default | Description |
|----------|---------|-------------|
| `WEBHOOK_SIGNING_SECRET` | _(required)_ | HMAC key shared with the signer. |
| `WEBHOOK_ADDR` | `:8080` | Public listener bind address. |
| `WEBHOOK_ADMIN_ADDR` | `:9090` | Admin listener bind address. |
| `WEBHOOK_READ_TIMEOUT` | `5s` | Full request read timeout. |
| `WEBHOOK_READ_HEADER_TIMEOUT` | `2s` | Header read timeout (slow-loris guard). |
| `WEBHOOK_WRITE_TIMEOUT` | `10s` | Response write timeout. |
| `WEBHOOK_IDLE_TIMEOUT` | `60s` | Keepalive idle timeout. |
| `WEBHOOK_SHUTDOWN_TIMEOUT` | `25s` | Drain budget on SIGTERM/SIGINT. |
| `WEBHOOK_MAX_BODY_BYTES` | `1048576` (1 MiB) | Body-size cap. |
| `WEBHOOK_SIGNATURE_HEADER` | `X-Webhook-Signature` | Header carrying `sha256=<hex>`. |
| `WEBHOOK_TIMESTAMP_HEADER` | `X-Webhook-Timestamp` | Header carrying Unix seconds. |
| `WEBHOOK_TIMESTAMP_SKEW` | `5m` | Allowed `\|now − ts\|` window. |
| `WEBHOOK_RATE_LIMIT_RPS` | `100` | Per-provider, per-replica RPS. |
| `WEBHOOK_RATE_LIMIT_BURST` | `200` | Per-provider burst. |
| `WEBHOOK_LOG_LEVEL` | `info` | One of `debug`, `info`, `warn`, `error`. |
| `WEBHOOK_LOG_FORMAT` | `json` | `json` or `text`. |
| `WEBHOOK_TRACING_ENABLED` | `true` | Enable OTLP/HTTP span export. |
| `WEBHOOK_TRACING_SAMPLE_RATIO` | `1.0` | Parent-based ratio sampler in `[0,1]`. |
| `WEBHOOK_PPROF_ENABLED` | `true` | Mount `/debug/pprof/*` on the admin mux. |
| `OTEL_SERVICE_NAME` | `webhookd` | Resource attribute. |
| `OTEL_SERVICE_VERSION` | `""` | Resource attribute. |
| `OTEL_EXPORTER_OTLP_*` | _(SDK defaults)_ | Read by the OTel SDK directly. |
| `WEBHOOK_PROVIDERS` | `jsm` | Comma-separated list of enabled providers; gates required provider-specific config. |
| `WEBHOOK_KUBECONFIG` | _empty_ | Optional kubeconfig path; empty falls back to in-cluster config. |
| `WEBHOOK_JSM_TRIGGER_STATUS` | `Ready to Provision` | JSM ticket status that fires the action. |
| `WEBHOOK_JSM_FIELD_PROVIDER_GROUP_ID` | _(required when JSM enabled)_ | JSM custom-field ID for the SSO group name. |
| `WEBHOOK_JSM_FIELD_ROLE` | _(required when JSM enabled)_ | JSM custom-field ID for the role name. |
| `WEBHOOK_JSM_FIELD_PROJECT` | _(required when JSM enabled)_ | JSM custom-field ID for the project name. |
| `WEBHOOK_CR_NAMESPACE` | `wiz-operator` | Namespace where CRs are applied. |
| `WEBHOOK_CR_API_GROUP` | `wiz.webhookd.io` | Operator CRD group; sanity-checked against imported types at startup. |
| `WEBHOOK_CR_API_VERSION` | `v1alpha1` | Operator CRD version. |
| `WEBHOOK_CR_FIELD_MANAGER` | `webhookd` | SSA `fieldManager` identity. |
| `WEBHOOK_CR_SYNC_TIMEOUT` | `20s` | Max time to wait for the CR to reach `Ready=True`. Must be `<` `WEBHOOK_SHUTDOWN_TIMEOUT`. |
| `WEBHOOK_CR_IDENTITY_PROVIDER_ID` | _(required when JSM enabled)_ | Static Wiz IdP identifier stamped onto every CR. |

## JSM provider

The JSM provider takes a Jira Service Management `jira:issue_updated`
webhook, verifies its HMAC signature, extracts three configured custom
fields, and SSA-applies a `SAMLGroupMapping` CR in the configured namespace.

### Configure the JSM side

Wire JSM's automation rule to POST to `https://<webhookd-host>/webhook/jsm`
with the following headers and body:

- `Content-Type: application/json`
- `X-Webhook-Timestamp: {{= new Date().getTime() / 1000 | floor}}`
- `X-Webhook-Signature: sha256=<HMAC-SHA256 of "v0:" + ts + ":" + body>`

The HMAC scheme is the project-wide v0 contract (see [Signing format](#signing-format)
above). Your JSM tenant computes the HMAC in an automation script — the
webhook body must be the canonical JSON sent below.

Required custom fields on the JSM ticket type, configurable by ID:

- `WEBHOOK_JSM_FIELD_PROVIDER_GROUP_ID` — the SSO group name (becomes
  `spec.providerGroupId`)
- `WEBHOOK_JSM_FIELD_ROLE` — the Wiz role name (becomes `spec.roleRef.name`)
- `WEBHOOK_JSM_FIELD_PROJECT` — the Wiz project name (becomes
  `spec.projectRefs[0].name`)

Tickets are processed only when `issue.fields.status.name` matches
`WEBHOOK_JSM_TRIGGER_STATUS` (default `Ready to Provision`); other statuses
return `200 noop` so the JSM automation rule advances cleanly.

### Response body

Every JSM response carries this shape:

```json
{
  "status": "success",
  "crName": "jsm-sec-1234",
  "namespace": "wiz-operator",
  "observedGeneration": 1,
  "traceId": "0af7651916cd43dd8448eb211c80319c",
  "requestId": "019dd3be-e2ad-7b27-8f0f-ba608b1ea2b4"
}
```

`status` is one of `success` | `noop` | `failure`. `traceId` is the W3C
trace ID also stamped onto the CR's `webhookd.io/trace-id` annotation for
cross-process trace correlation (see
[ADR-0007](docs/adr/0007-trace-context-propagation-via-cr-annotation.md)).
The JSM automation rule can include `traceId` and `crName` in the ticket
comment so SREs investigating failures pivot directly into Tempo.

### Failure-mode → JSM behavior

| Outcome | HTTP | `status` | JSM behavior |
|---------|------|----------|--------------|
| Ready=True observed | 200 | `success` | Add success comment, transition ticket. |
| Wrong status | 200 | `noop` | Advance without retry. |
| Bad JSON / missing field | 400 | `failure` | Add failure comment; tenant fixes ticket and retriggers. |
| Wrong field type | 422 | `failure` | Add failure comment; tenant fixes JSM custom-field config. |
| RBAC denied | 500 | `failure` | Page on-call; webhookd's RBAC is wrong. |
| Transient K8s error | 503 | `failure` | JSM retry policy retries. |
| Sync timeout | 504 | `failure` | JSM retry policy retries; CR was applied so retries are idempotent. |

## Deployment

webhookd ships as a single static binary plus three small Kubernetes
manifests for RBAC. The canonical CRDs live with the operator
(`github.com/donaldgifford/wiz-operator`) — webhookd's `deploy/crds/`
fixtures are minimal envtest schemas, not deployable definitions.

```bash
# Apply RBAC: ServiceAccount in `webhookd` ns, Role + RoleBinding in
# `wiz-operator` ns granting get/list/watch/patch on samlgroupmappings.
kubectl apply -k deploy/rbac/
```

The Phase 2 environment variables in the table below configure JSM custom
fields, the operator namespace, the sync budget, and the static identity
provider id stamped onto every CR.

## Observability

**Metrics** (DESIGN-0001 §Metrics) are exposed on `/metrics` from a private
registry — no global default registerer:

- HTTP: `webhookd_http_requests_total`, `webhookd_http_request_duration_seconds`,
  `webhookd_http_request_size_bytes`, `webhookd_http_response_size_bytes`,
  `webhookd_http_inflight_requests`, `webhookd_http_panics_total`,
  `webhookd_http_rate_limited_total`
- Webhook domain: `webhookd_webhook_events_total`,
  `webhookd_webhook_signature_validation_total`,
  `webhookd_webhook_processing_duration_seconds`
- K8s (Phase 2): `webhookd_k8s_apply_total{kind,outcome}` — outcome
  ∈ `created|updated|unchanged|error`;
  `webhookd_k8s_sync_duration_seconds{kind,outcome}` — outcome
  ∈ `ready|timeout|transient`
- JSM (Phase 2): `webhookd_jsm_payload_parse_errors_total{reason}`,
  `webhookd_jsm_noop_total{trigger_status}`,
  `webhookd_jsm_response_total{status_code}`
- Provenance: `webhookd_build_info{version,commit,go_version}`
- Plus `go_*` and `process_*` from the standard collectors

**Tracing** uses the OTel Go SDK with the OTLP/HTTP exporter (see
[ADR-0002](docs/adr/0002-prometheus-metrics-otel-tracing-split.md)). The SDK
reads `OTEL_EXPORTER_OTLP_*` natively. Spans are propagated via W3C
TraceContext + Baggage.

**Logging** uses `log/slog`. Every log line emitted with a context that
carries an active OTel span automatically gains `trace_id` and `span_id`
attributes via a small wrapper handler — no instrumentation at call sites.

## Development

The toolchain is pinned in `mise.toml` (Go 1.26.1, golangci-lint 2.11.4, etc.).
Run `mise install` to materialize it.

```bash
make build         # builds build/bin/webhookd with version ldflags
make test          # go test -v -race ./...
make test-pkg PKG=./internal/webhook
make test-coverage # writes coverage.out
make lint          # golangci-lint (Uber Go Style Guide)
make fmt           # gofmt + goimports + gofumpt + gci + golines
make ci            # lint + test + build + license-check
make license-check # allowlist check via go-licenses
make run-local     # build + run local binary
```

The fuzz target lives at `internal/webhook.FuzzSignatureVerify`:

```bash
go test -run='^$' -fuzz=FuzzSignatureVerify -fuzztime=30s ./internal/webhook
```

## Architecture

The service is split into single-purpose `internal/` packages plus the
`cmd/webhookd` entry point that wires them together:

- `internal/config` — environment-variable parsing and validation.
- `internal/observability` — slog with trace correlation, OTel tracer
  provider, Prometheus registry + `Metrics` struct.
- `internal/httpx` — middleware (Recover, OTel, RequestID, SLog, Metrics,
  RateLimit), admin mux, server constructor with config-driven timeouts.
- `internal/webhook` — HMAC + timestamp verification (the trust boundary),
  per-provider HTTP handler.
- `cmd/webhookd/main.go` — five-phase startup, dual-listener dispatch,
  graceful shutdown.

For the full design, decisions, and implementation log:

- [DESIGN-0001](docs/design/0001-stateless-webhook-receiver-phase-1.md) — Phase 1 receiver.
- [DESIGN-0002](docs/design/0002-jsm-webhook-to-samlmapping-provisioning-phase-2.md) — Phase 2 (planned).
- [IMPL-0001](docs/impl/0001-phase-1-stateless-receiver-implementation.md) — Phase 1 task list (Complete).
- [docs/adr/](docs/adr/) — settled architectural decisions (routing, observability split, env-only config, controller-runtime, SSA, sync response).

## License

Apache-2.0. Every Go source file carries an SPDX header; see
[LICENSE](LICENSE) and [licenses-header.txt](licenses-header.txt).
