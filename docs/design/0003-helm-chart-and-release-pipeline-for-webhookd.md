---
id: DESIGN-0003
title: "Helm Chart and Release Pipeline for webhookd"
status: In Review
author: Donald Gifford
created: 2026-04-28
---
<!-- markdownlint-disable-file MD025 MD041 -->

# DESIGN 0003: Helm Chart and Release Pipeline for webhookd

**Status:** In Review
**Author:** Donald Gifford
**Date:** 2026-04-28

<!--toc:start-->
- [Overview](#overview)
- [Goals and Non-Goals](#goals-and-non-goals)
  - [Goals](#goals)
  - [Non-Goals](#non-goals)
- [Background](#background)
- [Detailed Design](#detailed-design)
  - [Repository layout](#repository-layout)
  - [Chart metadata (Chart.yaml)](#chart-metadata-chartyaml)
  - [Template inventory](#template-inventory)
  - [Values schema](#values-schema)
  - [Cross-namespace RBAC](#cross-namespace-rbac)
  - [Secrets handling](#secrets-handling)
  - [Provider configuration](#provider-configuration)
  - [Observability provisioning](#observability-provisioning)
  - [CRD dependency](#crd-dependency)
  - [Release pipeline](#release-pipeline)
- [API / Interface Changes](#api--interface-changes)
- [Data Model](#data-model)
- [Testing Strategy](#testing-strategy)
- [Migration / Rollout Plan](#migration--rollout-plan)
- [Resolved Decisions](#resolved-decisions)
- [References](#references)
<!--toc:end-->

## Overview

Package webhookd's deployment manifests into a versioned Helm chart published
from this repo, plus a CI/CD pipeline that lints, unit-tests, and releases the
chart. The chart replaces the loose `deploy/rbac/` raw-manifest path as the
primary deployment surface and gives operators a single `helm install` command
to stand the service up against an existing wiz-operator install.

## Goals and Non-Goals

### Goals

- **A single `charts/webhookd/` chart** that fully deploys webhookd:
  Deployment, Service, ServiceAccount, cross-namespace Role/RoleBinding into
  the operator's namespace, optional ServiceMonitor, optional NetworkPolicy.
- **A `values.yaml` schema** that exposes the operationally meaningful knobs
  (image, replicas, resources, all `WEBHOOK_*` config, provider-specific
  config under nested keys, secret references, observability toggles) and
  validates them via a JSON schema (`values.schema.json`).
- **CI workflow** that runs `helm lint`, `helm template` smoke tests,
  `helm-unittest`, and `chart-testing` (`ct lint` + `ct install` against a
  kind cluster) on every PR that touches `charts/`.
- **Release workflow** that publishes the chart to GitHub Pages via
  `helm/chart-releaser-action`, mirroring repo-guardian's pattern. Optional
  OCI push to ghcr.io behind a feature toggle.
- **A `README.md.gotmpl`** rendered by `helm-docs` so the chart's own
  `README.md` stays in sync with `values.yaml` annotations on every PR.
- **Replace `deploy/rbac/` as the primary install path.** Keep the raw
  manifests as fixtures for envtest only; the README points users at
  `helm install` for cluster install.

### Non-Goals

- **Owning the wiz-operator's CRDs.** The `samlgroupmappings.wiz.webhookd.io`
  CRD ships with wiz-operator. webhookd's chart treats the CRD as a
  prerequisite (documented + optionally pre-flight-checked), not something
  it installs. This matches IMPL-0002's design: webhookd is a *consumer*.
- **Bundling wiz-operator.** No subchart, no umbrella dependency. Operators
  install wiz-operator through its own chart (or whatever mechanism that
  team ships) before webhookd.
- **A separate chart repository.** The chart lives under `charts/` in this
  repo, not in a dedicated `webhookd-charts` repo. Same model as
  repo-guardian.
- **Multi-tenancy / multi-instance support in one release.** One Helm release
  = one webhookd Deployment in one namespace, watching CRs in one target
  namespace. Two operators' worth of CRs = two `helm install` invocations.

## Background

**Current state (post-IMPL-0002):** webhookd ships raw manifests under
`deploy/rbac/` (ServiceAccount, Role, RoleBinding, kustomization) and CRD
fixtures under `deploy/crds/`. The README's "Deployment" section tells
operators to `kubectl apply -k deploy/rbac/` plus apply a Deployment they
write themselves. That works for a development install but doesn't scale to
multiple environments (dev / staging / prod) where image tag, replicas,
resource limits, and provider-specific signing secrets all diverge.

**The reference pattern** is
[repo-guardian](https://github.com/donaldgifford/repo-guardian) — same
author, similar deployment shape (Go binary, image at
`ghcr.io/donaldgifford/<name>`, distroless). repo-guardian's `charts/`
directory and `.github/workflows/chart-release.yml` are the model this
design largely mirrors. Concretely:

| Element | repo-guardian | webhookd (proposed) |
|---|---|---|
| Chart path | `charts/repo-guardian/` | `charts/webhookd/` |
| Templates | Deployment, Service, SA, ConfigMap, Secret, ServiceMonitor | Same set, plus cross-ns Role/RoleBinding |
| Tests | `tests/*_test.yaml` (helm-unittest) | Mirror |
| CI lint | `helm lint` + `helm-unittest` + `ct lint` + kind install | Mirror |
| Release | `helm/chart-releaser-action` to gh-pages, optional OCI | Mirror, OCI deferred |
| Docs | `README.md.gotmpl` rendered by `helm-docs` | Mirror |

**Differences webhookd needs to handle that repo-guardian doesn't:**

1. **Cross-namespace RBAC.** webhookd's ServiceAccount lives in its release
   namespace; the Role + RoleBinding granting `get/list/watch/patch` on
   `samlgroupmappings.wiz.webhookd.io` live in the **operator's** namespace
   (default `wiz-operator`). Helm charts default to one namespace; the
   cross-namespace bind has to be explicit in the template.
2. **CRD prerequisite, not CRD owner.** webhookd's chart depends on a CRD
   it doesn't ship. This is documented + optionally pre-flight-checked.
3. **Provider-gated config.** `WEBHOOK_PROVIDERS` is an allow-list that
   gates which provider-specific env vars become required at startup. The
   chart needs to render only the env vars for enabled providers.

## Detailed Design

### Repository layout

New top-level `charts/` directory:

```
charts/
└── webhookd/
    ├── .helmignore
    ├── Chart.yaml
    ├── values.yaml
    ├── values.schema.json        # JSON schema validating values
    ├── README.md                 # generated from README.md.gotmpl
    ├── README.md.gotmpl          # helm-docs source
    ├── ci/
    │   └── ci-values.yaml        # ct install override values (signing secret etc.)
    ├── tests/
    │   ├── deployment_test.yaml
    │   ├── service_test.yaml
    │   ├── serviceaccount_test.yaml
    │   ├── role_test.yaml
    │   ├── rolebinding_test.yaml
    │   ├── secret_test.yaml
    │   └── servicemonitor_test.yaml
    └── templates/
        ├── _helpers.tpl
        ├── NOTES.txt
        ├── deployment.yaml
        ├── service.yaml
        ├── serviceaccount.yaml
        ├── role.yaml                 # in target namespace (cross-ns)
        ├── rolebinding.yaml          # in target namespace (cross-ns)
        ├── secret.yaml               # signing secret (gated)
        ├── servicemonitor.yaml       # gated by metrics.serviceMonitor.enabled
        ├── networkpolicy.yaml        # gated by networkPolicy.enabled
        ├── poddisruptionbudget.yaml  # gated by podDisruptionBudget.enabled
        └── crd-precheck-job.yaml     # pre-install hook; gated by crdPrecheck.enabled
```

Plus a top-level `ct.yaml` for chart-testing config (mirrors repo-guardian).

### Chart metadata (`Chart.yaml`)

```yaml
apiVersion: v2
name: webhookd
description: Webhook receiver that provisions Wiz operator CRs from JSM payloads
type: application
version: 0.1.0          # chart SemVer; bumped per chart change
appVersion: "0.1.0"     # binary release version; tracks goreleaser tags
icon: https://raw.githubusercontent.com/donaldgifford/webhookd/main/.github/icon.png
home: https://github.com/donaldgifford/webhookd
sources:
  - https://github.com/donaldgifford/webhookd
keywords:
  - webhook
  - jsm
  - kubernetes-operator
  - saml
maintainers:
  - name: Donald Gifford
    email: dgifford06@gmail.com
```

**Versioning policy:** chart `version` and `appVersion` move
**independently**. Every chart-only change (template tweak, values
addition, helmunittest fix) bumps chart `version` even if `appVersion`
stays the same. `appVersion` only moves when a new binary release tags
out of goreleaser; Renovate auto-PRs the bump. This pre-1.0 split lets
us iterate on chart shape without waiting for binary releases. We can
revisit lockstep at 1.0 if churn settles down.

### Template inventory

| Template | Purpose | Gating |
|---|---|---|
| `deployment.yaml` | Pod spec, container ports (8080 webhook, 9090 admin), env vars from values + secret refs, probes, resources, securityContext | always |
| `service.yaml` | ClusterIP Service, two named ports `webhook` + `admin` | always |
| `serviceaccount.yaml` | ServiceAccount in release namespace | `serviceAccount.create=true` |
| `role.yaml` | Role in `targetNamespace` granting `get/list/watch/patch` on `samlgroupmappings.wiz.webhookd.io` | `rbac.create=true` |
| `rolebinding.yaml` | RoleBinding in `targetNamespace` binding the Role to the SA | `rbac.create=true` |
| `secret.yaml` | Inline-created Secret holding `WEBHOOK_SIGNING_SECRET` | `signing.createSecret=true` |
| `servicemonitor.yaml` | Prometheus Operator ServiceMonitor scraping the admin port | `metrics.serviceMonitor.enabled=true` |
| `networkpolicy.yaml` | Default-deny + allow ingress to webhook + admin ports from configured CIDRs | `networkPolicy.enabled=true` |
| `poddisruptionbudget.yaml` | PDB protecting availability during voluntary disruptions | `podDisruptionBudget.enabled=true` |
| `crd-precheck-job.yaml` | Pre-install/upgrade Helm hook that fails fast if `samlgroupmappings.wiz.webhookd.io` (or any other listed CRD) is missing; ships with its own SA + ClusterRole | `crdPrecheck.enabled=true` (default on) |
| `_helpers.tpl` | `webhookd.fullname`, `webhookd.labels`, `webhookd.selectorLabels`, `webhookd.serviceAccountName`, `webhookd.enabledProviders` | n/a |
| `NOTES.txt` | Post-install hints: webhook URL, signature header, link to docs | always |

### Values schema

The value tree, with proposed defaults:

```yaml
# image
image:
  repository: ghcr.io/donaldgifford/webhookd
  tag: ""                   # defaults to .Chart.AppVersion
  pullPolicy: IfNotPresent
imagePullSecrets: []

# pod / deployment
replicaCount: 1             # default; bump to 2+ for HA via values
podAnnotations: {}
podLabels: {}
podSecurityContext:
  runAsNonRoot: true
  runAsUser: 65532          # distroless nonroot
  fsGroup: 65532
securityContext:
  readOnlyRootFilesystem: true
  allowPrivilegeEscalation: false
  capabilities:
    drop: [ALL]

# resource limits — same defaults the README documents
resources:
  requests:
    cpu: 50m
    memory: 64Mi
  limits:
    cpu: 200m
    memory: 256Mi

# probes — exposed for tuning
livenessProbe:
  httpGet:
    path: /healthz
    port: admin
  initialDelaySeconds: 5
  periodSeconds: 10
readinessProbe:
  httpGet:
    path: /readyz
    port: admin
  initialDelaySeconds: 2
  periodSeconds: 5

# service
service:
  type: ClusterIP
  webhookPort: 8080
  adminPort: 9090

# ServiceAccount
serviceAccount:
  create: true
  annotations: {}
  name: ""                  # defaults to webhookd.fullname

# RBAC (target namespace bind)
rbac:
  create: true
  # Namespace where wiz-operator + the SAMLGroupMapping CRs live.
  # The chart writes Role + RoleBinding into this namespace, even
  # though the release itself is installed elsewhere.
  targetNamespace: wiz-operator

# Webhookd application config — maps 1:1 to WEBHOOK_* env vars
config:
  # core (DESIGN-0001)
  port: 8080
  adminPort: 9090
  shutdownTimeout: 30s
  bodyMaxBytes: 1048576
  rateLimit:
    enabled: true
    rps: 50
    burst: 100
  # tracing (optional)
  tracing:
    enabled: false
    endpoint: ""            # OTLP/HTTP endpoint
    sampleRatio: 1.0
  # pprof admin endpoint
  pprof:
    enabled: false

# Provider toggles. Each provider gets its own top-level block with
# its own `enabled` flag; WEBHOOK_PROVIDERS is computed at template
# time from whichever ones are enabled. Future providers (slack,
# github, etc.) will land as sibling blocks alongside `jsm:`.
jsm:
  enabled: true
  triggerStatus: "Approved"
  fieldProviderGroupID: "customfield_10001"
  fieldRole: "customfield_10002"
  fieldProject: "customfield_10003"
  syncTimeout: 20s
  crNamespace: ""           # defaults to rbac.targetNamespace
  crIdentityProviderID: ""  # required when jsm.enabled=true

# Signing secret (HMAC)
signing:
  # Name of an existing Secret containing the signing key.
  # Required if createSecret=false.
  existingSecret: ""
  existingSecretKey: webhookSecret
  # Inline-create a Secret from the value below. NEVER commit a real
  # secret in values.yaml; pass via --set-string or a sealed/external secret.
  createSecret: false
  secret: ""

# Observability
metrics:
  serviceMonitor:
    enabled: false
    interval: 30s
    scrapeTimeout: 10s
    labels: {}              # extra labels for the Prometheus Operator selector

# Network policy (default-deny + allow listed sources)
networkPolicy:
  enabled: false
  ingress:
    fromCIDRs: []           # e.g. ["10.0.0.0/8"]
    fromNamespaces: []      # e.g. [{ matchLabels: { team: jsm } }]

# PodDisruptionBudget — protect availability during voluntary disruptions
# (node drains, rolling upgrades). Default off because a single-replica
# install can't satisfy `minAvailable: 1`; bump replicaCount first.
podDisruptionBudget:
  enabled: false
  minAvailable: 1           # mutually exclusive with maxUnavailable
  maxUnavailable: ""

# CRD prerequisite check — runs as a pre-install/pre-upgrade Helm hook
# that verifies required CRDs exist before any chart manifest applies.
# Fails fast with a clear error if a CRD is missing instead of letting
# the Pod CrashLoop on first request.
crdPrecheck:
  enabled: true
  required:
    - samlgroupmappings.wiz.webhookd.io

# Pod scheduling
nodeSelector: {}
tolerations: []
affinity: {}
topologySpreadConstraints: []
```

A `values.schema.json` will mirror this with type validation and required-
field marking (e.g. `signing.existingSecret` is required when
`signing.createSecret=false`; `jsm.crIdentityProviderID` is required when
`jsm.enabled=true`).

### Cross-namespace RBAC

The hard problem. The default Helm install model:
`helm install webhookd ./charts/webhookd -n webhookd --create-namespace`
puts every namespaced object into `-n webhookd`. But our Role + RoleBinding
need to land in `wiz-operator` so the operator's apiserver-side authorization
checks find them.

Helm allows this via explicit `metadata.namespace` on each manifest; the
release-namespace default only applies when the manifest doesn't specify
one. So:

```yaml
# templates/role.yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: {{ include "webhookd.fullname" . }}
  namespace: {{ .Values.rbac.targetNamespace }}
  labels:
    {{- include "webhookd.labels" . | nindent 4 }}
rules:
  - apiGroups: ["wiz.webhookd.io"]
    resources: ["samlgroupmappings"]
    verbs: ["get", "list", "watch", "patch"]
```

```yaml
# templates/rolebinding.yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: {{ include "webhookd.fullname" . }}
  namespace: {{ .Values.rbac.targetNamespace }}
  labels:
    {{- include "webhookd.labels" . | nindent 4 }}
subjects:
  - kind: ServiceAccount
    name: {{ include "webhookd.serviceAccountName" . }}
    namespace: {{ .Release.Namespace }}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: {{ include "webhookd.fullname" . }}
```

**Caveat (open question 2):** the user installing the chart must have RBAC
permission to *create* a Role + RoleBinding in `targetNamespace`. In
multi-tenant clusters where the webhookd team doesn't own `wiz-operator`,
this is a problem. Options:

- Document the requirement; fail at install time if missing.
- Optional `rbac.useClusterRole: true` mode that uses a ClusterRole instead
  (requires cluster-admin to install but no cross-ns hop).
- Split installation into a "prep" phase (cluster-admin runs RBAC apply
  separately) and an "install" phase (webhookd team runs `helm install`
  with `rbac.create=false`).

Default to documented requirement + `rbac.create=true` + `rbac.create=false`
toggle to skip RBAC entirely for the prep-phase pattern.

### Secrets handling

The signing secret is the only sensitive value. Three patterns operators
will use:

1. **Inline-create** (`signing.createSecret=true`, `signing.secret=...`).
   Convenient for `--set-string` from CI; never commit values.yaml with a
   real secret. Discouraged but supported.
2. **Reference an existing Secret** (`signing.existingSecret=my-secret`,
   `signing.existingSecretKey=key`). The standard pattern; pairs with
   external-secrets / sealed-secrets / cluster secret stores.
3. **Reference + generated name** if neither is set. Chart fails with
   `helm template` validation rather than ship a default secret.

Deployment env-var wiring:

```yaml
env:
  - name: WEBHOOK_SIGNING_SECRET
    valueFrom:
      secretKeyRef:
        name: {{ if .Values.signing.createSecret }}
                {{ include "webhookd.fullname" . }}-signing
              {{ else }}
                {{ required "signing.existingSecret is required when createSecret=false" .Values.signing.existingSecret }}
              {{ end }}
        key: {{ .Values.signing.existingSecretKey }}
```

### Provider configuration

Each provider is its own top-level values block with an `enabled` flag.
`WEBHOOK_PROVIDERS` is computed at template time from whichever flags are
on. A `_helpers.tpl` template lists them once so multiple consumers
(env-var rendering, `values.schema.json` validation hints, NOTES.txt)
agree on the truth:

```gotemplate
{{- /* _helpers.tpl */ -}}
{{- define "webhookd.enabledProviders" -}}
{{- $providers := list -}}
{{- if .Values.jsm.enabled -}}{{- $providers = append $providers "jsm" -}}{{- end -}}
{{- /* future: if .Values.slack.enabled, etc. */ -}}
{{- join "," $providers -}}
{{- end -}}
```

The Deployment then renders provider env vars guarded on the same flag:

```yaml
env:
  - name: WEBHOOK_PROVIDERS
    value: {{ include "webhookd.enabledProviders" . | quote }}
  {{- if .Values.jsm.enabled }}
  - name: WEBHOOK_JSM_TRIGGER_STATUS
    value: {{ .Values.jsm.triggerStatus | quote }}
  - name: WEBHOOK_JSM_FIELD_PROVIDER_GROUP_ID
    value: {{ .Values.jsm.fieldProviderGroupID | quote }}
  - name: WEBHOOK_JSM_FIELD_ROLE
    value: {{ .Values.jsm.fieldRole | quote }}
  - name: WEBHOOK_JSM_FIELD_PROJECT
    value: {{ .Values.jsm.fieldProject | quote }}
  - name: WEBHOOK_CR_SYNC_TIMEOUT
    value: {{ .Values.jsm.syncTimeout | quote }}
  - name: WEBHOOK_CR_NAMESPACE
    value: {{ default .Values.rbac.targetNamespace .Values.jsm.crNamespace | quote }}
  - name: WEBHOOK_CR_IDENTITY_PROVIDER_ID
    value: {{ required "jsm.crIdentityProviderID is required when jsm.enabled=true" .Values.jsm.crIdentityProviderID | quote }}
  {{- end }}
```

This extends cleanly as future providers (`slack`, `github`, etc.) land —
each gets a sibling top-level block alongside `jsm:` and a corresponding
branch in `enabledProviders` + Deployment.

> **Why flat over nested?** A nested `providers.jsm.*` keeps everything
> "provider stuff" in one place but adds a level of indentation for every
> override (`--set providers.jsm.triggerStatus=Closed` reads worse than
> `--set jsm.triggerStatus=Closed`). The flat layout matches how
> `serviceAccount`, `rbac`, and `metrics` already live at top level.

### Observability provisioning

`ServiceMonitor` template gated by `metrics.serviceMonitor.enabled`. When
enabled it points at the `admin` port:

```yaml
{{- if .Values.metrics.serviceMonitor.enabled }}
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: {{ include "webhookd.fullname" . }}
  labels:
    {{- include "webhookd.labels" . | nindent 4 }}
    {{- with .Values.metrics.serviceMonitor.labels }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
spec:
  endpoints:
    - port: admin
      path: /metrics
      interval: {{ .Values.metrics.serviceMonitor.interval }}
      scrapeTimeout: {{ .Values.metrics.serviceMonitor.scrapeTimeout }}
  selector:
    matchLabels:
      {{- include "webhookd.selectorLabels" . | nindent 6 }}
{{- end }}
```

Tracing is purely env-var driven; no template machinery. Operators set
`config.tracing.enabled=true` + `config.tracing.endpoint=...` and webhookd
exports OTLP/HTTP per ADR-0002.

### CRD dependency

The chart **cannot install** the `samlgroupmappings.wiz.webhookd.io` CRD
because that CRD's authoritative source is the wiz-operator project (per
IMPL-0002 §Resolved Decisions §1). The chart enforces the prerequisite
via a pre-install Helm hook so a missing CRD fails fast with a clear
error rather than CrashLooping the Pod on first request.

```yaml
# templates/crd-precheck-job.yaml
{{- if .Values.crdPrecheck.enabled }}
apiVersion: batch/v1
kind: Job
metadata:
  name: {{ include "webhookd.fullname" . }}-crd-precheck
  labels:
    {{- include "webhookd.labels" . | nindent 4 }}
  annotations:
    "helm.sh/hook": pre-install,pre-upgrade
    "helm.sh/hook-weight": "-5"
    "helm.sh/hook-delete-policy": before-hook-creation,hook-succeeded
spec:
  backoffLimit: 0
  ttlSecondsAfterFinished: 60
  template:
    spec:
      restartPolicy: Never
      serviceAccountName: {{ include "webhookd.fullname" . }}-crd-precheck
      containers:
        - name: precheck
          image: bitnami/kubectl:latest
          command: ["/bin/sh", "-ec"]
          args:
            - |
              for crd in {{ join " " .Values.crdPrecheck.required }}; do
                echo "Checking for CRD $crd..."
                kubectl get crd "$crd" >/dev/null \
                  || { echo "ERROR: CRD $crd not found. Install wiz-operator first."; exit 1; }
              done
              echo "All required CRDs present."
{{- end }}
```

A separate ServiceAccount + cluster-scoped Role (`get` on `crds`) is
created with the same hook annotations so the precheck Job has the
narrow permission it needs.

The required CRD list is values-driven so the hook stays useful as the
operator's CRD set evolves:

```yaml
crdPrecheck:
  enabled: true
  required:
    - samlgroupmappings.wiz.webhookd.io
```

When the wiz-operator renames or splits a CRD, only the values list
changes — the chart template stays the same, and operators can override
the list at install time without waiting for a chart release. If the
precheck becomes brittle in practice (e.g. installs against clusters
where the CRD is being applied in the same operation), `crdPrecheck.enabled=false`
turns it off and falls back to documented prerequisites.

When wiz-operator eventually publishes its own chart, the precheck Job
becomes redundant and we'll switch to a `Chart.yaml` dependency. Tracked
as a follow-up; not blocking this design.

### Release pipeline

Two GitHub Actions workflows mirroring repo-guardian's pattern:

**`.github/workflows/chart-ci.yml`** — runs on PRs touching `charts/**`:

```yaml
name: Chart CI
on:
  pull_request:
    paths: ["charts/**", "ct.yaml", ".github/workflows/chart-ci.yml"]
jobs:
  lint:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6
      - uses: azure/setup-helm@v4
      - run: helm lint charts/webhookd

  unittest:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6
      - uses: d3adb5/helm-unittest-action@v2
        with:
          charts: charts/webhookd

  ct:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6
        with: { fetch-depth: 0 }
      - uses: azure/setup-helm@v4
      - uses: actions/setup-python@v5
        with: { python-version: "3.12" }
      - uses: helm/chart-testing-action@v2
      - run: ct lint --config ct.yaml
      - uses: helm/kind-action@v1
      - run: |
          # Apply the wiz-operator CRD fixtures so install succeeds
          kubectl apply -f deploy/crds/samlgroupmapping.yaml
          ct install --config ct.yaml
```

**`.github/workflows/chart-release.yml`** — manual `workflow_dispatch`,
publishes to **OCI on ghcr.io as the primary distribution**, and to
gh-pages as a parallel/secondary path so users on either toolchain are
covered:

```yaml
name: Chart Release
on:
  workflow_dispatch: {}
permissions:
  contents: write
  packages: write              # required for OCI push to ghcr.io
jobs:
  release-oci:
    name: Push chart to OCI registry (ghcr.io)
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v6
        with: { fetch-depth: 0 }
      - uses: azure/setup-helm@v4
      - name: Login to ghcr.io
        run: |
          echo "${{ secrets.GITHUB_TOKEN }}" | \
            helm registry login ghcr.io -u "${{ github.actor }}" --password-stdin
      - name: Package and push
        run: |
          helm package charts/webhookd -d /tmp/charts
          helm push /tmp/charts/webhookd-*.tgz oci://ghcr.io/donaldgifford/charts

  release-gh-pages:
    name: Publish chart to GitHub Pages (chart-releaser)
    runs-on: ubuntu-latest
    needs: release-oci         # OCI is primary; gh-pages mirrors
    steps:
      - uses: actions/checkout@v6
        with: { fetch-depth: 0 }
      - run: |
          git config user.name "github-actions[bot]"
          git config user.email "github-actions[bot]@users.noreply.github.com"
      - uses: azure/setup-helm@v4
      - uses: helm/chart-releaser-action@v1
        with:
          charts_dir: charts
          skip_existing: true
        env:
          CR_TOKEN: ${{ secrets.GITHUB_TOKEN }}
```

Operators install via the **OCI path** (preferred):

```bash
helm install webhookd oci://ghcr.io/donaldgifford/charts/webhookd \
  --version 0.1.0 \
  -n webhookd --create-namespace
```

…or the gh-pages path:

```bash
helm repo add webhookd https://donaldgifford.github.io/webhookd
helm repo update
helm install webhookd webhookd/webhookd -n webhookd --create-namespace
```

Why ship both: OCI is the strategic direction (Helm's native registry
support, no separate repo URL to share, plays nicely with the same
authn that pulls the container image), but gh-pages is universally
supported by older Helm versions and tools that haven't adopted OCI.
The chart-release workflow ships both atomically — same `Chart.yaml`,
same `.tgz` content — so users picking either path get the same chart.

A separate `.github/workflows/helm-docs.yml` runs `helm-docs` on PR and
fails if the chart's `README.md` is out of sync with `values.yaml`
annotations. This forces docs to stay current.

**Renovate** (`.github/renovate.json`) is wired from day one to track
`appVersion` against goreleaser tags. Each new `webhookd` binary
release auto-opens a PR bumping `Chart.yaml`'s `appVersion` (and chart
`version` per the chart-only-bump policy below). Mirrors repo-guardian's
config.

## API / Interface Changes

The chart's `values.yaml` becomes a public API surface. Breaking changes
to `values.yaml` (renaming fields, removing fields, changing default
behavior) bump the chart's major version. Compatible additions (new
optional fields with safe defaults) bump the minor.

The CLI surface (`helm install`, `helm upgrade`, `helm uninstall`) is
unchanged from any other chart.

## Data Model

No persistent data. The chart deploys a stateless service whose only
state is the runtime application (in-memory rate limiter, OTel batch
processor, in-flight HTTP requests). Cluster-side state lives in
`SAMLGroupMapping` CRs that webhookd creates but does not own.

## Testing Strategy

- **`helm lint charts/webhookd`** — schema validation, template syntax,
  best-practice checks. Runs in CI on every PR.
- **`helm-unittest`** with templates under `charts/webhookd/tests/*.yaml`:
  - Default install renders the expected manifest set.
  - `serviceAccount.create=false` skips the ServiceAccount template.
  - `signing.createSecret=false` requires `signing.existingSecret`; missing
    value fails with the documented error.
  - `metrics.serviceMonitor.enabled=true` renders ServiceMonitor; default
    omits it.
  - `rbac.targetNamespace=foo` puts Role + RoleBinding in `foo`, SA stays
    in release namespace.
  - `config.providers` excludes jsm → JSM env vars are absent.
- **`ct lint`** + **`ct install`** in CI against a kind cluster with the
  `samlgroupmappings.wiz.webhookd.io` CRD pre-applied from
  `deploy/crds/samlgroupmapping.yaml`. Verifies the chart renders, installs,
  the Pod reaches `Ready=True`, and `helm test` (a connection-test Pod hits
  `/healthz`) passes.
- **`helm-docs`** drift check — fails if `README.md` is stale relative to
  `values.yaml` annotations.

## Migration / Rollout Plan

**Phase 1: chart parity with `deploy/rbac/`.** Implement the chart so that
`helm install` produces functionally-equivalent manifests to
`kubectl apply -k deploy/rbac/` plus a hand-written Deployment.
Deliverables: `charts/webhookd/` (all templates including the
`crd-precheck-job`, PDB, NetworkPolicy, ServiceMonitor), `chart-ci.yml`
workflow, `helm-unittest` cases, README pointer.

**Phase 2: release pipeline + Renovate.** `chart-release.yml` shipping
both OCI (ghcr.io, primary) and gh-pages (mirror). `helm-docs.yml` drift
check. `.github/renovate.json` tracking `appVersion` against goreleaser
tags. First release: chart `version: 0.1.0`, `appVersion: 0.1.0`. Verify
both `helm install oci://...` and `helm repo add` paths work.

**Phase 3: deprecate `deploy/rbac/`.** Update README to point primary
install at the chart; mark `deploy/rbac/` as fixture-only (it already is
labeled this way at the top of each YAML file). Keep `deploy/crds/` for
envtest.

**Phase 4 (follow-up, not blocking):** When wiz-operator publishes its
own chart, replace the `crd-precheck-job` Helm hook with a `Chart.yaml`
dependency on the operator's chart. Smaller code surface, no
ad-hoc kubectl Job.

Rollback: chart releases are append-only on gh-pages; an operator
pinning to a previous version (`helm install ... --version 0.0.1`)
always works. Chart YAML edits that get reviewed and merged but not
released don't ship until someone clicks `workflow_dispatch`.

## Resolved Decisions

These started as open questions during drafting and have been answered.
Recorded here so future readers can see the reasoning rather than just
the outcome (mirrors IMPL-0001 and IMPL-0002).

1. **Charts directory layout: `charts/webhookd/`.** Mirror repo-guardian
   exactly. Confirmed; no alternative considered worth the divergence.

2. **Cross-namespace RBAC: support both modes via `rbac.create`.** The
   chart bundles `templates/role.yaml` + `templates/rolebinding.yaml`
   that target `rbac.targetNamespace` (default `wiz-operator`) and gates
   them on `rbac.create`. When the installing user has cross-ns
   permissions, the default `true` works. When they don't (multi-tenant
   cluster, separation between webhookd team and operator team),
   `--set rbac.create=false` skips the RBAC bundle and the cluster-admin
   pre-applies it out of band. Both first-class. Documented in the
   chart README's "Installing in a multi-tenant cluster" section.

3. **CRD prerequisite: pre-install hook (default on, values-driven
   list).** Bundle a `crd-precheck-job` template gated by
   `crdPrecheck.enabled=true` (default). Job runs as a
   `pre-install,pre-upgrade` Helm hook with a narrow ServiceAccount +
   ClusterRole (`get` on `customresourcedefinitions`). Required CRD
   list lives in `crdPrecheck.required` so operators can amend it as
   the operator's CRD set evolves without waiting for a chart release.
   `crdPrecheck.enabled=false` falls back to documented prerequisites
   (useful for cluster-bootstrap scenarios where the CRD is being
   applied in the same operation). When wiz-operator publishes its
   own chart, this gets replaced by a `Chart.yaml` dependency
   (Phase 4 deferral).

4. **Signing secret: both modes.** `signing.existingSecret` (preferred,
   pairs with sealed-secrets / external-secrets / cluster secret
   stores) AND `signing.createSecret` (convenient for `--set-string`
   from CI). `values.schema.json` enforces that exactly one is
   configured. Documented preference for `existingSecret` in the
   chart README.

5. **Provider ergonomics: flatter top-level blocks.** `jsm.enabled`,
   `jsm.triggerStatus`, etc. at top level rather than nested under
   `providers.jsm.*`. Cleaner `--set jsm.foo=bar` ergonomics, and matches
   how `serviceAccount`, `rbac`, `metrics`, `networkPolicy`, and
   `podDisruptionBudget` already live at top level. `WEBHOOK_PROVIDERS`
   is computed at template time from a `webhookd.enabledProviders` helper
   that walks the known provider blocks. Future providers (`slack`,
   `github`, etc.) get sibling blocks alongside `jsm:`.

6. **ServiceMonitor: bundled, default off.** `templates/servicemonitor.yaml`
   gated by `metrics.serviceMonitor.enabled=false`. Mirror repo-guardian.
   Off by default because not every cluster runs Prometheus Operator;
   operators with it flip the toggle.

7. **Publishing: OCI primary, gh-pages secondary.** Both ship from the
   same `chart-release.yml` workflow on every release —
   `oci://ghcr.io/donaldgifford/charts/webhookd` is the strategic
   direction (native Helm OCI support, single-source authn with the
   container image), and `https://donaldgifford.github.io/webhookd`
   serves the same `.tgz` for users on older Helm versions or tools
   without OCI support. `release-gh-pages` `needs: release-oci` so
   if OCI fails, gh-pages doesn't ship a divergent state.

8. **Versioning: chart-only bumps allowed pre-1.0.** Chart `version`
   moves on every chart-only change (template tweak, values addition,
   helmunittest fix), independent of `appVersion`. `appVersion` only
   moves when a new binary release tags out of goreleaser; Renovate
   auto-PRs the bump. Revisit lockstep at 1.0 if churn settles.

9. **Renovate from day one.** `.github/renovate.json` ships in Phase 2
   tracking `appVersion` against goreleaser tags, mirroring
   repo-guardian's config. Avoids hand-bumping `appVersion` on every
   binary release.

10. **NetworkPolicy: default off.** `networkPolicy.enabled=false`
    default. Many clusters don't run a NetworkPolicy controller (CNI
    not enforcing), and a NetworkPolicy that isn't enforced silently
    misleads operators about their actual security posture. Operators
    on enforcing CNIs flip the toggle and configure
    `networkPolicy.ingress.fromCIDRs` / `fromNamespaces`.

11. **`replicaCount`: default 1.** Single-replica is the simplest thing
    that works for a stateless service; HA is opt-in via values. Pairs
    naturally with `podDisruptionBudget.enabled=false` default — bumping
    replicas and enabling the PDB are usually done together when an
    operator is ready for HA.

12. **PodDisruptionBudget: bundled, default off.** `templates/poddisruptionbudget.yaml`
    gated by `podDisruptionBudget.enabled=false`. Off by default
    because a single-replica install can't satisfy `minAvailable: 1`
    and would block voluntary disruptions (node drain) until a manual
    override. Operators bumping `replicaCount` typically enable the
    PDB at the same time.

## References

- **Reference implementation:**
  [donaldgifford/repo-guardian](https://github.com/donaldgifford/repo-guardian) —
  same author, similar deployment shape, source of the chart layout +
  workflows we're mirroring.
- **Skill guidance:**
  - `helm:helm` — opinionated chart authoring (chart structure, values
    design, toolchain).
  - `helm:helm-chart-repo` — chart repo management with `ct`, `cr`,
    Renovate, helm-docs.
  - `helm:lint` — chart-testing + helm lint runtime.
- **Helm tools:**
  - [chart-releaser-action](https://github.com/helm/chart-releaser-action)
    for gh-pages release publishing.
  - [chart-testing](https://github.com/helm/chart-testing) for `ct lint`
    and `ct install` in CI.
  - [helm-unittest](https://github.com/helm-unittest/helm-unittest) for
    template unit tests.
  - [helm-docs](https://github.com/norwoodj/helm-docs) for
    `README.md.gotmpl` rendering.
- **Webhookd context:**
  - DESIGN-0001 — stateless receiver baseline; defines the env-var matrix.
  - DESIGN-0002 — JSM → SAMLGroupMapping provisioning; defines the RBAC
    requirements and CRD prerequisite.
  - IMPL-0002 §Resolved Decisions — canonical CRD shape, RBAC verbs,
    target namespace.
  - ADR-0007 — trace context propagation (relevant when
    `config.tracing.enabled=true`).
