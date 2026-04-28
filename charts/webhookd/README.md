# webhookd

![Version: 0.1.0](https://img.shields.io/badge/Version-0.1.0-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: 0.1.0](https://img.shields.io/badge/AppVersion-0.1.0-informational?style=flat-square)

Stateless HTTP webhook receiver that maps signed payloads to Kubernetes custom resources.

**Homepage:** <https://github.com/donaldgifford/webhookd>

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

----------------------------------------------
Autogenerated from chart metadata using [helm-docs v1.14.2](https://github.com/norwoodj/helm-docs/releases/v1.14.2)
