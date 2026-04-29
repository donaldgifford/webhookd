# webhookd

Stateless HTTP webhook receiver that maps signed payloads to Kubernetes custom resources.

![Version: 0.1.0](https://img.shields.io/badge/Version-0.1.0-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: 0.1.0](https://img.shields.io/badge/AppVersion-0.1.0-informational?style=flat-square)

**Homepage:** <https://github.com/donaldgifford/webhookd>

## Installation

### OCI Registry (recommended)

```bash
helm install webhookd \
  oci://ghcr.io/donaldgifford/charts/webhookd \
  --version 0.1.0 \
  --namespace webhookd \
  --create-namespace \
  --set jsm.crIdentityProviderID=YOUR_IDP_ID \
  --set signing.createSecret=true \
  --set signing.secret=$(openssl rand -hex 32)
```

### GitHub Pages mirror

```bash
helm repo add webhookd https://donaldgifford.github.io/webhookd
helm repo update
helm install webhookd webhookd/webhookd \
  --version 0.1.0 \
  --namespace webhookd \
  --create-namespace \
  -f values.yaml
```

## Prerequisites

- Kubernetes Kubernetes: `>=1.30.0`
- Helm 3.16+
- The `samlgroupmappings.wiz.webhookd.io` CRD installed in the cluster
  (the wiz-operator owns the schema). The chart's pre-install
  `crd-precheck` Job verifies presence and fails fast otherwise; set
  `crdPrecheck.enabled=false` to skip the gate.
- An HMAC signing secret. Use either `signing.existingSecret` (preferred
  — supports External Secrets Operator, 1Password Connect, etc.) or
  `signing.createSecret=true` to inline-create the Secret from a
  one-shot value.

## Multi-instance install

Because the chart writes Role + RoleBinding into `rbac.targetNamespace`
rather than the release namespace, you can host multiple webhookd
releases in distinct release namespaces while pointing them at
distinct operator namespaces. Each release uses a unique
`fullnameOverride` to avoid Role-name collisions:

```bash
helm install webhookd-jsm-prod oci://ghcr.io/donaldgifford/charts/webhookd \
  --namespace webhookd-jsm-prod \
  --create-namespace \
  --set fullnameOverride=webhookd-jsm-prod \
  --set rbac.targetNamespace=wiz-operator-prod \
  --set jsm.crIdentityProviderID=prod-idp \
  --set signing.existingSecret=webhookd-jsm-prod-signing
```

## Observability

- Prometheus metrics: enabled by default on the admin port at
  `/metrics`. Set `metrics.serviceMonitor.enabled=true` to render a
  Prometheus Operator `ServiceMonitor`.
- OpenTelemetry traces: set `config.tracing.enabled=true` and point
  `config.tracing.endpoint` at an OTLP/HTTP collector. The Pod will
  emit a `OTEL_EXPORTER_OTLP_ENDPOINT` env var to pick it up.
- pprof: gated separately via `config.pprof.enabled`. Off in
  production.

## Hardening

- `networkPolicy.enabled=true` renders a default-deny ingress with
  allow-listed CIDRs and namespace selectors.
- `podDisruptionBudget.enabled=true` protects availability during node
  drains; bump `replicaCount` to ≥ 2 first so `minAvailable: 1` can
  actually be satisfied.
- The Pod runs as a non-root distroless user (uid/gid 65532) with
  read-only root FS and all Linux capabilities dropped by default.
  A `/tmp` emptyDir provides scratch for the OTel batch processor and
  signal-handler temporaries.

## Maintainers

| Name | Email | Url |
| ---- | ------ | --- |
| Donald Gifford |  | <https://github.com/donaldgifford> |

## Source Code

* <https://github.com/donaldgifford/webhookd>

## Requirements

Kubernetes: `>=1.30.0`

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| affinity | object | `{}` | Affinity rules applied to the webhookd Pod. |
| config.adminPort | int | `9090` | TCP port the admin listener binds to. Mirror with `service.adminPort`. |
| config.bodyMaxBytes | int | `1048576` | Reject webhook bodies larger than this (1 MiB by default). |
| config.port | int | `8080` | TCP port the webhook listener binds to. Mirror with `service.webhookPort`. |
| config.pprof.enabled | bool | `false` | Expose `/debug/pprof/*` on the admin listener. |
| config.rateLimit.burst | int | `100` | Burst capacity per provider. |
| config.rateLimit.enabled | bool | `true` | Toggle the per-provider token-bucket rate limiter. |
| config.rateLimit.rps | int | `50` | Steady-state requests-per-second per provider. |
| config.shutdownTimeout | string | `"30s"` | Maximum time the server has to drain in-flight work on SIGTERM. |
| config.tracing.enabled | bool | `false` | Enable OpenTelemetry traces. |
| config.tracing.endpoint | string | `""` | OTLP/HTTP endpoint. |
| config.tracing.sampleRatio | float | `1` | Sampler ratio in `[0.0, 1.0]`. `1.0` traces every request. |
| crdPrecheck.enabled | bool | `true` | Run the pre-install CRD-presence check Job. |
| crdPrecheck.image.pullPolicy | string | `"IfNotPresent"` | Pull policy for the precheck image. |
| crdPrecheck.image.repository | string | `"cgr.dev/chainguard/kubectl"` | Container image used by the precheck Job. Chainguard's distroless kubectl is the default. |
| crdPrecheck.image.tag | string | `"latest-dev"` | Tag for the precheck image. |
| crdPrecheck.required | list | `["samlgroupmappings.wiz.webhookd.io"]` | CRDs that must exist before the chart applies its templates. |
| fullnameOverride | string | `""` | Override the fully qualified app name. Defaults to `Release.Name-Chart.Name`. |
| image.pullPolicy | string | `"IfNotPresent"` | Image pull policy. |
| image.repository | string | `"ghcr.io/donaldgifford/webhookd"` | Container image repository. |
| image.tag | string | `""` | Container image tag. Defaults to `.Chart.AppVersion` when empty. |
| imagePullSecrets | list | `[]` | List of references to secrets in the same namespace used to pull the image. |
| jsm.crIdentityProviderID | string | `""` | IDP id stamped on `spec.identityProviderID`. **Required** when `jsm.enabled=true`. |
| jsm.crNamespace | string | `""` | Namespace into which `SAMLGroupMapping` CRs are written. Defaults to `rbac.targetNamespace` when empty. |
| jsm.enabled | bool | `true` | Enable the JSM webhook → `SAMLGroupMapping` provider. |
| jsm.fieldProject | string | `"customfield_10003"` | JSM custom-field ID carrying the project payload. |
| jsm.fieldProviderGroupID | string | `"customfield_10001"` | JSM custom-field ID carrying the SAML group/provider ID payload. |
| jsm.fieldRole | string | `"customfield_10002"` | JSM custom-field ID carrying the role payload. |
| jsm.syncTimeout | string | `"20s"` | Maximum time to wait for the operator to mark the CR `Ready=True`. Must be < `config.shutdownTimeout`. |
| jsm.triggerStatus | string | `"Approved"` | Trigger value of `request.currentStatus.statusName` that maps to a CR apply. |
| livenessProbe | object | `{"httpGet":{"path":"/healthz","port":"admin"},"initialDelaySeconds":5,"periodSeconds":10}` | Liveness probe spec (passed verbatim into the Pod template). |
| metrics.serviceMonitor.enabled | bool | `false` | Render a Prometheus Operator `ServiceMonitor`. |
| metrics.serviceMonitor.interval | string | `"30s"` | Scrape interval. |
| metrics.serviceMonitor.labels | object | `{}` | Extra labels applied to the `ServiceMonitor` for Prometheus selectors. |
| metrics.serviceMonitor.scrapeTimeout | string | `"10s"` | Scrape timeout. |
| nameOverride | string | `""` | Override the chart name. Defaults to `Chart.Name`. |
| networkPolicy.enabled | bool | `false` | Render a default-deny + allow-listed-sources NetworkPolicy. |
| networkPolicy.ingress.fromCIDRs | list | `[]` | CIDR blocks allowed to reach the webhook + admin ports. |
| networkPolicy.ingress.fromNamespaces | list | `[]` | Namespace selectors allowed to reach the webhook + admin ports. |
| nodeSelector | object | `{}` | Node selector applied to the webhookd Pod. |
| podAnnotations | object | `{}` | Pod-level annotations. |
| podDisruptionBudget.enabled | bool | `false` | Render a PodDisruptionBudget. |
| podDisruptionBudget.maxUnavailable | string | `""` | Maximum number of pods that may become unavailable. Mutually exclusive with `minAvailable`. |
| podDisruptionBudget.minAvailable | int | `1` | Minimum number of pods that must remain available. Mutually exclusive with `maxUnavailable`. |
| podLabels | object | `{}` | Pod-level labels. |
| podSecurityContext.fsGroup | int | `65532` | File-system group applied to mounted volumes. |
| podSecurityContext.runAsNonRoot | bool | `true` | Run all containers as non-root users. |
| podSecurityContext.runAsUser | int | `65532` | Numeric UID the container runs as. `65532` matches the distroless `nonroot` user. |
| rbac.create | bool | `true` | Create the Role + RoleBinding for managing the webhookd CR(s). |
| rbac.targetNamespace | string | `"wiz-operator"` | Namespace where the wiz-operator and `SAMLGroupMapping` CRs live. The chart writes Role + RoleBinding into this namespace, even though the release itself is installed elsewhere. |
| readinessProbe | object | `{"httpGet":{"path":"/readyz","port":"admin"},"initialDelaySeconds":2,"periodSeconds":5}` | Readiness probe spec (passed verbatim into the Pod template). |
| replicaCount | int | `1` | Number of webhookd replicas. |
| resources.limits.cpu | string | `"200m"` | CPU limit. |
| resources.limits.memory | string | `"256Mi"` | Memory limit. |
| resources.requests.cpu | string | `"50m"` | CPU request. |
| resources.requests.memory | string | `"64Mi"` | Memory request. |
| securityContext.allowPrivilegeEscalation | bool | `false` | Block privilege escalation via setuid binaries. |
| securityContext.capabilities.drop | list | `["ALL"]` | Drop every Linux capability; webhookd needs none. |
| securityContext.readOnlyRootFilesystem | bool | `true` | Mount the container root filesystem read-only. |
| service.adminPort | int | `9090` | TCP port the Service exposes for the admin endpoints (metrics, probes, pprof). |
| service.type | string | `"ClusterIP"` | Service type for the webhookd-fronting Service. |
| service.webhookPort | int | `8080` | TCP port the Service exposes for inbound webhook traffic. |
| serviceAccount.annotations | object | `{}` | Annotations applied to the ServiceAccount (e.g. IAM role bindings). |
| serviceAccount.create | bool | `true` | Create a dedicated ServiceAccount for webhookd. |
| serviceAccount.name | string | `""` | Override the generated ServiceAccount name. |
| signing.createSecret | bool | `false` | Inline-create a Secret from the value below. **Never** commit a real secret to values.yaml. |
| signing.existingSecret | string | `""` | Name of an existing Secret containing the signing key. Required when `createSecret=false`. |
| signing.existingSecretKey | string | `"webhookSecret"` | Key in the existing Secret that holds the signing material. |
| signing.secret | string | `""` | Signing-key value used when `createSecret=true`. |
| tolerations | list | `[]` | Tolerations applied to the webhookd Pod. |
| topologySpreadConstraints | list | `[]` | Topology-spread constraints applied to the webhookd Pod. |

## Troubleshooting

- **Pod stuck in `CrashLoopBackOff` with `WEBHOOK_PROVIDERS` errors.**
  Either `WEBHOOK_PROVIDERS` is empty (set `jsm.enabled=true` or another
  provider) or a provider-required field is missing. Check `kubectl
  describe pod` for the exit reason.
- **Pre-install hook fails with "required CRDs not present".** The
  wiz-operator's CRD bundle isn't installed yet. Either install
  wiz-operator first or set `crdPrecheck.enabled=false` to skip the
  gate (the Pod will then CrashLoop on the first webhook instead).
- **`helm install` rejects values with a schema error.** webhookd's
  `values.schema.json` validates types and required-field
  cross-conditions (e.g. `jsm.crIdentityProviderID` required when
  `jsm.enabled=true`). Read the error message — it cites the offending
  path.
- **Role/RoleBinding lands in the wrong namespace.** Set
  `rbac.targetNamespace` to the namespace where the wiz-operator (and
  therefore the `SAMLGroupMapping` CRs) actually live. Default is
  `wiz-operator`.

## Links

- [DESIGN-0003](https://github.com/donaldgifford/webhookd/blob/main/docs/design/0003-helm-chart-and-release-pipeline-for-webhookd.md)
- [IMPL-0003](https://github.com/donaldgifford/webhookd/blob/main/docs/impl/0003-helm-chart-and-release-pipeline-implementation.md)
- [Project README](https://github.com/donaldgifford/webhookd#readme)

----

Autogenerated from chart metadata using [helm-docs](https://github.com/norwoodj/helm-docs).
