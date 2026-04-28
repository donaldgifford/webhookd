---
id: IMPL-0003
title: "Helm Chart and Release Pipeline Implementation"
status: Ready
author: Donald Gifford
created: 2026-04-28
---
<!-- markdownlint-disable-file MD025 MD041 -->

# IMPL 0003: Helm Chart and Release Pipeline Implementation

**Status:** Ready
**Author:** Donald Gifford
**Date:** 2026-04-28

<!--toc:start-->
- [Objective](#objective)
- [Scope](#scope)
  - [In Scope](#in-scope)
  - [Out of Scope](#out-of-scope)
- [Implementation Phases](#implementation-phases)
  - [Phase 0: Bootstrap & Toolchain](#phase-0-bootstrap--toolchain)
    - [Tasks](#tasks)
    - [Success Criteria](#success-criteria)
  - [Phase 1: Core Templates (Always-On)](#phase-1-core-templates-always-on)
    - [Tasks](#tasks-1)
    - [Success Criteria](#success-criteria-1)
  - [Phase 2: Optional / Gated Templates](#phase-2-optional--gated-templates)
    - [Tasks](#tasks-2)
    - [Success Criteria](#success-criteria-2)
  - [Phase 3: Values Schema & Generated Docs](#phase-3-values-schema--generated-docs)
    - [Tasks](#tasks-3)
    - [Success Criteria](#success-criteria-3)
  - [Phase 4: Chart CI Workflow](#phase-4-chart-ci-workflow)
    - [Tasks](#tasks-4)
    - [Success Criteria](#success-criteria-4)
  - [Phase 5: Release Pipeline & Renovate](#phase-5-release-pipeline--renovate)
    - [Tasks](#tasks-5)
    - [Success Criteria](#success-criteria-5)
  - [Phase 6: First Release & Smoke Verification](#phase-6-first-release--smoke-verification)
    - [Tasks](#tasks-6)
    - [Success Criteria](#success-criteria-6)
  - [Phase 7: README Rewrite & Deprecation](#phase-7-readme-rewrite--deprecation)
    - [Tasks](#tasks-7)
    - [Success Criteria](#success-criteria-7)
- [File Changes](#file-changes)
- [Testing Plan](#testing-plan)
- [Dependencies](#dependencies)
- [Resolved Decisions](#resolved-decisions)
- [References](#references)
<!--toc:end-->

## Objective

Land DESIGN-0003 in eight focused phases: a `charts/webhookd/` Helm chart
that fully replaces `deploy/rbac/` as the primary install path, plus the
CI/CD pipeline that lints, tests, and publishes the chart to OCI on
ghcr.io (primary) and gh-pages (mirror). Each phase is independently
shippable as its own commit; the full set takes webhookd from
"raw-manifest install" to "single `helm install` against a versioned
chart published from this repo."

**Implements:** [DESIGN-0003](../design/0003-helm-chart-and-release-pipeline-for-webhookd.md)

## Scope

### In Scope

- A complete `charts/webhookd/` directory with all 11 templates the
  design specifies (Deployment, Service, ServiceAccount, Role,
  RoleBinding, Secret, ServiceMonitor, NetworkPolicy, PodDisruptionBudget,
  CRD-precheck Job, NOTES + helpers).
- `values.yaml` matching the design's flat per-provider schema, plus
  a `values.schema.json` that enforces required-field cross-validation
  (e.g. `jsm.crIdentityProviderID` required when `jsm.enabled=true`).
- `helm-unittest` cases per template covering both the gated-on and
  gated-off paths.
- `chart-ci.yml` workflow on PRs touching `charts/**`: `helm lint`,
  `helm-unittest`, `chart-testing` (`ct lint` + `ct install` against
  kind with the wiz CRD pre-applied), and a `helm-docs` drift check.
- `chart-release.yml` workflow on `workflow_dispatch` shipping the
  same `.tgz` to OCI (`oci://ghcr.io/donaldgifford/charts/webhookd`)
  and gh-pages atomically.
- `.github/renovate.json` tracking `Chart.yaml`'s `appVersion` against
  the webhookd binary's goreleaser tags.
- README rewrite that flips primary deployment instructions to the
  chart and demotes `deploy/rbac/` to a fixture-only label.
- Helm tooling pinned in `mise.toml`: `helm`, `helm-unittest`,
  `helm-docs`, `chart-testing` (`ct`), `chart-releaser` (`cr`).

### Out of Scope

- A `Chart.yaml` dependency on a wiz-operator chart. wiz-operator
  hasn't published yet; the CRD-precheck Job is the substitute. When
  wiz-operator publishes, swapping the precheck for a real dependency
  is a follow-up (Phase 4 in the design's Migration Plan).
- Multi-instance / multi-tenancy support in one Helm release.
- A separate `webhookd-charts` repository. Chart lives under `charts/`
  in this repo.
- Bundling wiz-operator as a subchart.

## Implementation Phases

Each phase builds on the previous and ships as its own commit. A phase
is complete when all tasks are checked and the success criteria are met.

---

### Phase 0: Bootstrap & Toolchain

Establish the chart directory skeleton and pin the helm tooling so
every subsequent phase has a reproducible local + CI environment.
Nothing renders yet — this phase is purely structural.

#### Tasks

- [x] Add helm tooling to `mise.toml` (mirrors repo-guardian's
      pin set; exact patch versions for reproducibility):
  - [x] `helm = "3.19.0"`.
  - [x] `kubectl = "1.31.4"` (matches `ENVTEST_K8S_VERSION` in Makefile).
  - [x] `helm-cr = "1.8.1"` (chart-releaser CLI; used locally — CI uses
        the action).
  - [x] `helm-ct = "3.14.0"` (chart-testing CLI).
  - [x] `helm-diff = "3.15.0"` (used by `make helm-diff-check`).
  - [x] `helm-docs = "1.14.2"`.
- [x] Add a `make chart-tools` Makefile target that runs
      `helm plugin install https://github.com/helm-unittest/helm-unittest --version 1.0.3`
      (helm-unittest is shipped as a helm plugin, not as a standalone
      binary — same approach repo-guardian uses). Idempotent: re-runs
      no-op once installed.
- [x] Create `charts/webhookd/` directory.
- [x] `charts/webhookd/.helmignore` (standard exclusions: `.git`,
      `.github`, `*.md` outside chart, etc.).
- [ ] **Pre-Phase-0 prerequisite:** cut a `v0.1.0` binary release
      (`gh workflow run release.yml --ref main` after tagging) so the
      chart's `appVersion: 0.1.0` matches a real published image.
      Resolved Decision §1 — chart and binary versions stay aligned
      for the first release. **Status:** chart pins `appVersion: 0.1.0`
      ahead of the binary tag; user owns the release-time tag cut.
- [x] `charts/webhookd/Chart.yaml` with `apiVersion: v2`, `name: webhookd`,
      description, `type: application`, `version: 0.1.0`,
      `appVersion: 0.1.0` (matches the freshly-cut binary tag),
      `kubeVersion: ">=1.30.0"`, maintainers, sources, keywords,
      icon URL, plus ArtifactHub annotations.
- [x] `charts/webhookd/CHANGELOG.md` with a `## 0.1.0 - 2026-04-28`
      entry seeded from this initial release. Resolved Decision §11 —
      hand-curated per-chart changelog drives release-notes content.
- [x] `charts/webhookd/values.yaml` skeleton with **all** the value
      blocks from DESIGN-0003 §Values schema: `image`, `replicaCount`,
      `podSecurityContext`, `securityContext`, `resources`,
      `livenessProbe`, `readinessProbe`, `service`, `serviceAccount`,
      `rbac`, `config`, `jsm`, `signing`, `metrics.serviceMonitor`,
      `networkPolicy`, `podDisruptionBudget`, `crdPrecheck`,
      `nodeSelector`, `tolerations`, `affinity`, `topologySpreadConstraints`,
      `imagePullSecrets`, `podAnnotations`, `podLabels`,
      `nameOverride`, `fullnameOverride`. With helm-docs `# --` comments
      on every leaf so Phase 3's README generation works incrementally.
- [x] `charts/webhookd/templates/_helpers.tpl` with named templates:
  - [x] `webhookd.name`
  - [x] `webhookd.fullname`
  - [x] `webhookd.chart`
  - [x] `webhookd.labels`
  - [x] `webhookd.selectorLabels`
  - [x] `webhookd.serviceAccountName`
  - [x] `webhookd.targetNamespace`
  - [x] `webhookd.enabledProviders` (comma-joined list of provider
        names whose `enabled=true`; today just `jsm`).
  - [x] `webhookd.signingSecretName` / `webhookd.signingSecretKey`
        (helpers added during Phase 0 to keep deployment.yaml DRY in
        Phase 1).
- [x] `charts/webhookd/templates/NOTES.txt` skeleton (URLs, signing-
      header reminder, JSM-side webhook URL guidance, link to chart
      README).
- [x] `ct.yaml` at repo root (mirrors repo-guardian: `chart-dirs:
      [charts]`, `target-branch: main`, `check-version-increment: false`,
      `validate-maintainers: false`, `validate-chart-schema: false`,
      `lint-conf: charts/.yamllint.yml`).
- [x] `charts/.yamllint.yml` with the relaxations `ct lint` needs
      (line-length disabled, `truthy: warning`).
- [x] `charts/webhookd/ci/ci-values.yaml` empty placeholder (Phase 4
      fills it).
- [x] Update `Makefile` with `make helm-lint`, `make helm-test`,
      `make helm-unittest`, `make helm-ct-lint`, `make helm-ct-install`,
      `make helm-docs`, `make helm-docs-check`, `make helm-template`,
      `make helm-package`, `make helm-push` targets (mirrors
      repo-guardian's `helm-*` naming convention; supersedes the
      original `chart-*` draft naming).
- [x] Update CLAUDE.md with the chart layout note + `make helm-*`
      target list.

#### Success Criteria

- `make helm-lint` (`helm lint charts/webhookd`) runs cleanly
  (warnings allowed since no templates yet, but `Error` count = 0).
  ✅ — `1 chart(s) linted, 0 chart(s) failed`.
- `helm template charts/webhookd` renders nothing — `NOTES.txt` only
  renders on install, not via `helm template`. ✅ — empty render.
- `mise install` materializes all the new tools without manual steps.
  ✅
- `make helm-test` (lint + helm-unittest) is wired and `make helm-docs`
  generates a default README. ✅
- `git status` shows only the expected new files; no test-related
  changes leaking in. ✅

---

### Phase 1: Core Templates (Always-On)

The five always-on templates that produce a complete deploy parity
against `deploy/rbac/` plus a working Deployment. These are the heart
of the chart; gated templates come in Phase 2.

#### Tasks

- [ ] `templates/deployment.yaml`:
  - [ ] Full Pod spec: `replicas`, `selector`, named container
        `webhookd`, image from `.Values.image.{repository,tag,pullPolicy}`
        with `appVersion` fallback for tag.
  - [ ] Two named container ports: `webhook` (`config.port`, default
        8080) and `admin` (`config.adminPort`, default 9090).
  - [ ] `livenessProbe` + `readinessProbe` rendered from values, both
        targeting the `admin` named port (Phase 1 of DESIGN-0001 only
        ships `/healthz` on admin; `/readyz` is also on admin).
  - [ ] All core `WEBHOOK_*` env vars from `.Values.config.*`:
        `WEBHOOK_PORT`, `WEBHOOK_ADMIN_PORT`, `WEBHOOK_SHUTDOWN_TIMEOUT`,
        `WEBHOOK_BODY_MAX_BYTES`, `WEBHOOK_RATE_LIMIT_*`,
        `WEBHOOK_TRACING_*`, `WEBHOOK_PPROF_ENABLED`,
        `WEBHOOK_PROVIDERS` (computed from `webhookd.enabledProviders`
        helper), and `WEBHOOK_KUBECONFIG` empty for in-cluster.
  - [ ] Provider env-var block guarded by `{{- if .Values.jsm.enabled }}`:
        `WEBHOOK_JSM_*` and `WEBHOOK_CR_*` keys per IMPL-0002 config
        layout. `crIdentityProviderID` rendered with `required` so
        mis-config fails template-time.
  - [ ] `WEBHOOK_SIGNING_SECRET` from `secretKeyRef`, name resolved
        per `.Values.signing.{createSecret,existingSecret}`.
  - [ ] `securityContext` rendered from values (default: read-only root
        FS, run-as-non-root with uid/gid 65532, drop ALL capabilities,
        no privilege escalation).
  - [ ] `volumeMounts` for `/tmp` emptyDir (read-only root FS forces
        a writable tempdir for OTel batch processor + signal-handler
        scratch).
  - [ ] `resources`, `nodeSelector`, `tolerations`, `affinity`,
        `topologySpreadConstraints`, `imagePullSecrets`, `podAnnotations`,
        `podLabels` all rendered from values.
- [ ] `templates/service.yaml`:
  - [ ] `type: {{ .Values.service.type }}` (default ClusterIP).
  - [ ] Two named service ports: `webhook` → `webhookPort` →
        `webhook` targetPort, `admin` → `adminPort` → `admin`
        targetPort.
- [ ] `templates/serviceaccount.yaml` gated on `serviceAccount.create=true`.
- [ ] `templates/role.yaml`:
  - [ ] Gated on `rbac.create=true`.
  - [ ] `metadata.namespace: {{ .Values.rbac.targetNamespace }}` so it
        lands in `wiz-operator` (or override) regardless of release ns.
  - [ ] Rule: `apiGroups=[wiz.webhookd.io]`, `resources=[samlgroupmappings]`,
        `verbs=[get, list, watch, patch]` (matches IMPL-0002 RBAC).
- [ ] `templates/rolebinding.yaml`:
  - [ ] Same gate, same target ns.
  - [ ] Subject: SA in `.Release.Namespace`; roleRef: matching Role.
- [ ] `templates/secret.yaml`:
  - [ ] Gated on `signing.createSecret=true`.
  - [ ] `data.webhookSecret: {{ .Values.signing.secret | b64enc | quote }}`,
        with `required` enforcing presence.
- [ ] `tests/deployment_test.yaml` — helm-unittest cases:
  - [ ] Renders correct image (default = `Chart.AppVersion` fallback).
  - [ ] Custom `image.tag` overrides.
  - [ ] `replicaCount: 3` reflected in spec.
  - [ ] Provider env vars present when `jsm.enabled=true`.
  - [ ] `jsm.crIdentityProviderID=""` + `jsm.enabled=true` →
        template error containing the `required` message.
  - [ ] `WEBHOOK_PROVIDERS` value matches enabled-providers helper.
  - [ ] `signing.createSecret=true` + `signing.secret=foo` → secretKeyRef
        points at the chart-local Secret.
  - [ ] `signing.createSecret=false` + `signing.existingSecret=foo` →
        secretKeyRef points at `foo`; `existingSecret=""` →
        template error.
  - [ ] securityContext and resources reflected per values.
  - [ ] livenessProbe / readinessProbe target the admin port by name.
- [ ] `tests/service_test.yaml`:
  - [ ] Default ClusterIP type, two named ports (`webhook`, `admin`).
  - [ ] `service.type: LoadBalancer` reflected.
- [ ] `tests/serviceaccount_test.yaml`:
  - [ ] Created by default; `serviceAccount.create=false` skips.
  - [ ] `serviceAccount.name=foo` overrides.
- [ ] `tests/role_test.yaml`:
  - [ ] `metadata.namespace` equals `rbac.targetNamespace` value, not
        release namespace.
  - [ ] `rbac.create=false` skips.
  - [ ] verbs are exactly `[get, list, watch, patch]`.
- [ ] `tests/rolebinding_test.yaml`:
  - [ ] Subject namespace equals `.Release.Namespace`, not the
        target namespace.
  - [ ] `rbac.create=false` skips.
- [ ] `tests/secret_test.yaml`:
  - [ ] `signing.createSecret=true` + `signing.secret=foo` →
        Secret rendered with `webhookSecret` key.
  - [ ] `signing.createSecret=false` → no Secret rendered.

#### Success Criteria

- `helm template charts/webhookd \
    --set jsm.crIdentityProviderID=foo \
    --set signing.createSecret=true \
    --set signing.secret=bar \
    -n webhookd` produces functionally-equivalent manifests to
  `kubectl apply -k deploy/rbac/` plus a working Deployment.
- `helm install webhookd charts/webhookd -n webhookd --create-namespace` against
  a kind cluster with `deploy/crds/samlgroupmapping.yaml` pre-applied
  brings up a Pod that reaches `Ready=True`.
- `helm-unittest charts/webhookd` passes all Phase 1 cases.
- A POST to the deployed Pod's webhook URL with a signed JSM payload
  produces a `SAMLGroupMapping` CR in `wiz-operator` namespace.

---

### Phase 2: Optional / Gated Templates

The four feature-toggled templates plus the cluster-prerequisite
hook. These are independent enough that each could ship in its own
commit if Phase 1 lands first.

#### Tasks

- [ ] `templates/servicemonitor.yaml`:
  - [ ] Gated on `metrics.serviceMonitor.enabled=true`.
  - [ ] Endpoint targets the `admin` named port, path `/metrics`.
  - [ ] `interval` and `scrapeTimeout` from values.
  - [ ] Extra labels from `metrics.serviceMonitor.labels` for
        Prometheus Operator selectors.
- [ ] `templates/networkpolicy.yaml`:
  - [ ] Gated on `networkPolicy.enabled=true`.
  - [ ] Default-deny ingress; allow on the webhook + admin ports
        from `networkPolicy.ingress.fromCIDRs` and
        `networkPolicy.ingress.fromNamespaces`.
- [ ] `templates/poddisruptionbudget.yaml`:
  - [ ] Gated on `podDisruptionBudget.enabled=true`.
  - [ ] `minAvailable` OR `maxUnavailable` (mutually exclusive); template
        validation on values.
- [ ] `templates/crd-precheck-job.yaml` package — three resources behind
      one `crdPrecheck.enabled=true` gate (default on):
  - [ ] ClusterRole granting `get` on `customresourcedefinitions.apiextensions.k8s.io`,
        with `helm.sh/hook: pre-install,pre-upgrade`,
        `helm.sh/hook-weight: -10`,
        `helm.sh/hook-delete-policy: before-hook-creation,hook-succeeded`.
  - [ ] ServiceAccount in release namespace + ClusterRoleBinding,
        also pre-install/pre-upgrade with weight `-9`.
  - [ ] Job with weight `-5`, `backoffLimit: 0`, `ttlSecondsAfterFinished: 60`,
        running `kubectl` from
        `cgr.dev/chainguard/kubectl:latest-dev` (Resolved Decision §4 —
        Chainguard distroless, signed, supply-chain hardened); image
        repo + tag exposed via `crdPrecheck.image.{repository,tag}` so
        air-gapped users can override. Job command loops over
        `.Values.crdPrecheck.required` and exits non-zero with a
        descriptive error if any CRD is absent.
- [ ] `tests/servicemonitor_test.yaml`:
  - [ ] Default → not rendered.
  - [ ] `metrics.serviceMonitor.enabled=true` → rendered with admin
        port endpoint.
  - [ ] `metrics.serviceMonitor.labels` propagated.
- [ ] `tests/networkpolicy_test.yaml`:
  - [ ] Default → not rendered.
  - [ ] `networkPolicy.enabled=true` + sample CIDRs → ingress rule
        contains them.
- [ ] `tests/poddisruptionbudget_test.yaml`:
  - [ ] Default → not rendered.
  - [ ] `podDisruptionBudget.enabled=true` → rendered with default
        `minAvailable: 1`.
  - [ ] Setting both `minAvailable` and `maxUnavailable` → template
        error.
- [ ] `tests/crd-precheck-job_test.yaml`:
  - [ ] Default (`crdPrecheck.enabled=true`) → all three resources
        rendered with correct hook annotations and weights.
  - [ ] `crdPrecheck.enabled=false` → none rendered.
  - [ ] `crdPrecheck.required=[a,b,c]` → all three appear in the Job's
        command args.

#### Success Criteria

- `helm template charts/webhookd --set metrics.serviceMonitor.enabled=true ...`
  renders the ServiceMonitor; default does not.
- `helm install` against kind with `crdPrecheck.enabled=true` and the
  required CRD present succeeds; `helm install` with the required CRD
  *absent* fails the pre-install hook with a clear error and never
  applies the chart's other manifests.
- All Phase 2 helm-unittest cases pass.

---

### Phase 3: Values Schema & Generated Docs

Lock the values contract via JSON schema validation and start the
README.md.gotmpl pipeline so chart README stays in sync.

#### Tasks

- [ ] `charts/webhookd/values.schema.json`:
  - [ ] Type validation for every key in `values.yaml`.
  - [ ] Required-field cross-validation:
    - [ ] `jsm.crIdentityProviderID` required when `jsm.enabled=true`.
    - [ ] `signing.existingSecret` required when `signing.createSecret=false`.
    - [ ] `signing.secret` required when `signing.createSecret=true`.
    - [ ] `podDisruptionBudget.maxUnavailable` excluded when
          `podDisruptionBudget.minAvailable` set, and vice versa.
    - [ ] `crdPrecheck.required` non-empty when `crdPrecheck.enabled=true`.
  - [ ] `additionalProperties: false` at every object level so typos
        in `--set` paths fail loudly.
- [ ] Add helm-docs `# --` annotations to every leaf in `values.yaml`
      (groundwork done in Phase 0; this phase is the audit pass).
- [ ] `charts/webhookd/README.md.gotmpl` mirroring repo-guardian's:
      header, configuration table, install snippets (OCI + gh-pages),
      multi-tenant install pattern, observability config, hardening
      notes (NetworkPolicy + PDB + ServiceMonitor toggles), CRD
      prerequisite, signing-secret patterns, troubleshooting
      ("CrashLoopBackOff: check `WEBHOOK_PROVIDERS`"). Include
      `{{ template "chart.valuesSection" . }}`.
- [ ] Run `helm-docs` locally; commit the generated `charts/webhookd/README.md`.
- [ ] Schema-rejection helm-unittest cases:
  - [ ] `--set jsm.enabled=true --set jsm.crIdentityProviderID=""` →
        `helm install --dry-run` rejects with schema error.
  - [ ] `--set signing.createSecret=false --set signing.existingSecret=""` →
        same.
  - [ ] `--set unknownKey=foo` → `additionalProperties` rejection.
- [ ] Update `Makefile`'s `make chart-docs` target to re-run helm-docs
      and `git diff --exit-code charts/webhookd/README.md` so a
      missed regen fails the target locally before CI catches it.

#### Success Criteria

- `helm install --dry-run --debug charts/webhookd` against valid values
  succeeds; against any of the schema-violating inputs above, fails
  with a schema-validation error citing the offending field.
- `make chart-docs` produces no diff vs. the committed README.md.
- `charts/webhookd/README.md` rendered cleanly: every value block has
  a description, default, and type column.

---

### Phase 4: Chart CI Workflow

CI gating so chart changes can't merge without lint, unit tests, an
end-to-end install on kind, and a docs-drift check.

#### Tasks

- [ ] `charts/webhookd/ci/ci-values.yaml`:
  - [ ] `jsm.crIdentityProviderID: "ci-test"` (satisfies required-field).
  - [ ] `signing.createSecret: true` + `signing.secret: ci-test-secret`.
  - [ ] `crdPrecheck.enabled: true` (default; just for clarity).
  - [ ] `rbac.targetNamespace: ct-target` (so the test cluster can
        pre-create that ns + the CRD).
- [ ] `.github/workflows/chart-ci.yml`:
  - [ ] Trigger: `pull_request` on `paths: [charts/**, ct.yaml,
        .github/workflows/chart-ci.yml]`.
  - [ ] `lint` job: `helm lint charts/webhookd`.
  - [ ] `unittest` job: `d3adb5/helm-unittest-action@v2`.
  - [ ] `ct` job:
    - [ ] `helm/chart-testing-action@v2` with python setup.
    - [ ] `ct list-changed --config ct.yaml` to skip on no-op PRs.
    - [ ] `ct lint --config ct.yaml`.
    - [ ] `helm/kind-action@v1` for cluster.
    - [ ] `kubectl create namespace ct-target` before install.
    - [ ] `kubectl apply -f deploy/crds/samlgroupmapping.yaml` so the
          CRD-precheck Job passes.
    - [ ] `ct install --config ct.yaml` runs the install end-to-end
          (chart Pod must reach Ready, `helm test` Pod must hit
          `/healthz` successfully).
- [ ] `.github/workflows/helm-docs.yml`:
  - [ ] Trigger: `pull_request` on `paths: [charts/webhookd/values.yaml,
        charts/webhookd/README.md.gotmpl, charts/webhookd/Chart.yaml,
        .github/workflows/helm-docs.yml]`.
  - [ ] Runs `helm-docs --dry-run` and fails if any file would change.
- [ ] `pr-labels.yml` (existing) updated with a **path-based** rule
      mapping any PR touching `charts/**` (or `ct.yaml`,
      `charts/.yamllint.yml`) to a `chart` label. Resolved Decision §10
      — path-based mirrors webhookd's existing convention; branch-name
      heuristics don't.
- [ ] Smoke-test the workflow locally:
  - [ ] `act -j lint` (if act available) or trigger by pushing to
        the feature branch.
- [ ] Update CLAUDE.md with the chart CI testing patterns:
  - [ ] How to add a new helm-unittest case.
  - [ ] What `ct install` covers vs. helm-unittest.
  - [ ] When to bump `ct.yaml`'s lint config.

#### Success Criteria

- A PR that touches `charts/webhookd/templates/deployment.yaml` triggers
  `chart-ci.yml`; all four jobs (lint, unittest, ct, helm-docs) run.
- `ct install` against kind succeeds end-to-end with the
  `ci-values.yaml` overrides.
- Removing a `# --` annotation from `values.yaml` without re-running
  `helm-docs` produces a CI failure on `helm-docs.yml`.

---

### Phase 5: Release Pipeline & Renovate

The chart is now releasable. Wire publishing to OCI on ghcr.io as the
primary distribution and gh-pages as a parallel mirror, plus Renovate
for automatic `appVersion` tracking.

#### Tasks

- [ ] `.github/workflows/chart-release.yml`:
  - [ ] Trigger: `workflow_dispatch` only (manual release).
  - [ ] Permissions: `contents: write` (for gh-pages branch),
        `packages: write` (for OCI push).
  - [ ] `release-oci` job:
    - [ ] `helm registry login ghcr.io` with `${{ github.actor }}` +
          `${{ secrets.GITHUB_TOKEN }}`.
    - [ ] `helm package charts/webhookd -d /tmp/charts`.
    - [ ] `helm push /tmp/charts/webhookd-*.tgz oci://ghcr.io/donaldgifford/charts`.
  - [ ] `release-gh-pages` job:
    - [ ] `needs: release-oci` so a failed OCI push blocks the
          gh-pages mirror.
    - [ ] `helm/chart-releaser-action@v1` with `charts_dir: charts`,
          `skip_existing: true`, `CR_TOKEN: ${{ secrets.GITHUB_TOKEN }}`.
          Resolved Decision §7 — let the action create the orphan
          `gh-pages` branch on first run; no pre-seed step.
  - [ ] Release-notes step that extracts the latest entry from
        `charts/webhookd/CHANGELOG.md` (everything between the top
        `## X.Y.Z` heading and the next `## ` heading) and passes it
        to `chart-releaser-action` via the `--release-notes-file` flag.
        Resolved Decision §11 — per-chart hand-written CHANGELOG drives
        release notes; no git-cliff coupling.
- [ ] `.github/renovate.json`:
  - [ ] Mirror repo-guardian's config: extends `config:base`, enables
        `helm-values`, custom regex manager that watches goreleaser
        tags on this repo and bumps `charts/webhookd/Chart.yaml`'s
        `appVersion` field.
  - [ ] Group all helm-related updates into one PR.
  - [ ] Optional: dependency dashboard issue.
- [ ] OCI registry visibility flip moved to Phase 6 (one-time manual
      step after the very first push — Resolved Decision §8).
- [ ] Sigstore / cosign signing of the `.tgz` is **deferred** to a
      follow-up (Resolved Decision §9). README's install instructions
      do not advertise `--verify` for v0.1.0.

#### Success Criteria

- `gh workflow run chart-release.yml` against the merged feat branch
  publishes `webhookd-0.1.0.tgz` to **both** OCI and gh-pages.
- `helm pull oci://ghcr.io/donaldgifford/charts/webhookd --version 0.1.0`
  succeeds from a fresh machine without auth.
- `helm repo add webhookd https://donaldgifford.github.io/webhookd`
  resolves; `helm search repo webhookd` returns chart 0.1.0.
- Renovate opens a PR within 24h of a hypothetical `v0.0.3` binary tag,
  bumping `Chart.yaml`'s `appVersion`.

---

### Phase 6: First Release & Smoke Verification

Cut chart 0.1.0 against a fresh kind cluster end-to-end via both
install paths.

#### Tasks

- [ ] **Verify** `v0.1.0` binary tag and ghcr.io image exist before
      releasing the chart. Resolved Decision §1 — chart and binary
      ship at the same version for the first release.
- [ ] `gh workflow run chart-release.yml --ref main` (after merge).
- [ ] **One-time** ghcr.io package visibility flip after the first
      OCI push: navigate to `https://github.com/users/donaldgifford/packages/container/charts%2Fwebhookd/settings`
      and change visibility from Private → Public. Resolved
      Decision §8 — once-per-package; not worth automating.
- [ ] OCI install smoke test on a fresh kind cluster:
  - [ ] `kind create cluster --name webhookd-oci-test`.
  - [ ] `kubectl apply -f deploy/crds/samlgroupmapping.yaml`.
  - [ ] `kubectl create namespace webhookd && kubectl create namespace wiz-operator`.
  - [ ] `helm install webhookd oci://ghcr.io/donaldgifford/charts/webhookd \
          --version 0.1.0 -n webhookd \
          --set jsm.crIdentityProviderID=smoke \
          --set signing.createSecret=true \
          --set signing.secret=smoketest`.
  - [ ] Verify: chart-precheck Job completed successfully; webhookd
        Pod reaches Ready; admin port `/healthz` returns OK.
  - [ ] POST a signed JSM payload to the Service (port-forwarded);
        verify a `SAMLGroupMapping` lands in `wiz-operator` namespace.
- [ ] gh-pages install smoke test on a separate kind cluster:
  - [ ] `helm repo add webhookd https://donaldgifford.github.io/webhookd`.
  - [ ] `helm install webhookd webhookd/webhookd ...` with the same
        overrides.
  - [ ] Same end-to-end assertions.
- [ ] `helm uninstall webhookd -n webhookd` cleans up both installs
      without leaving orphaned Role/RoleBinding in `wiz-operator` ns.
- [ ] Document the smoke-test commands in `docs/runbook/release-checklist.md`
      (new file, points back at this phase).

#### Success Criteria

- Both install paths succeed against a fresh cluster; Pod reaches
  Ready; webhook produces a CR.
- `helm uninstall` leaves zero residue (no orphaned Role,
  RoleBinding, or precheck SA/ClusterRole).
- `docs/runbook/release-checklist.md` is the one place future-us
  re-runs to verify a release.

---

### Phase 7: README Rewrite & Deprecation

Flip the user-facing install story to chart-first; demote `deploy/rbac/`
to fixture-only.

#### Tasks

- [ ] `README.md` "Deployment" section rewritten:
  - [ ] Lead with `helm install oci://ghcr.io/donaldgifford/charts/webhookd
        --version <X.Y.Z> -n webhookd --create-namespace ...`.
  - [ ] Show the gh-pages alternative immediately below.
  - [ ] Cross-link DESIGN-0003 + IMPL-0003.
  - [ ] Remove the "kubectl apply -k deploy/rbac/" instructions; replace
        with a small "Raw manifests (envtest fixtures only)" pointer.
- [ ] `deploy/rbac/*.yaml` headers updated with a banner comment:
      `# deploy/rbac/ is an envtest fixture only. Production installs
      use the Helm chart at charts/webhookd/.`
- [ ] `deploy/crds/*.yaml` headers stay as-is (already labeled fixture-only).
- [ ] CLAUDE.md project state paragraph extended:
  - [ ] Phase 3 milestone listed alongside Phases 1-2.
  - [ ] Active branch language flipped from `feat/helm-chart` to
        post-merge `main`.
  - [ ] Architectural patterns: chart layout, OCI-primary publishing,
        precheck-hook pattern.
- [ ] DESIGN-0003 status flipped to `Implemented`.
- [ ] This doc's status flipped to `Complete`.

#### Success Criteria

- A new contributor reading the README can install webhookd with one
  `helm install` command in under 60 seconds (assuming kind +
  wiz-operator's CRD).
- `kubectl apply -k deploy/rbac/` no longer appears as the primary path
  anywhere in user-facing docs.
- Doc indexes (`docs/design/README.md`, `docs/impl/README.md`) reflect
  Implemented / Complete status.

---

## File Changes

| File | Action | Description |
|------|--------|-------------|
| `mise.toml` | Modify | Pin helm + helm-unittest + helm-docs + chart-testing + chart-releaser. |
| `Makefile` | Modify | Add `chart-lint`, `chart-test`, `chart-docs` targets. |
| `ct.yaml` | Create | Repo-root chart-testing config. |
| `charts/.yamllint.yml` | Create | Yamllint relaxations for chart-testing. |
| `charts/webhookd/Chart.yaml` | Create | Chart metadata, version, appVersion. |
| `charts/webhookd/CHANGELOG.md` | Create | Per-chart hand-curated changelog; release notes source. |
| `charts/webhookd/.helmignore` | Create | Standard helm exclusions. |
| `charts/webhookd/values.yaml` | Create | Full values schema with helm-docs annotations. |
| `charts/webhookd/values.schema.json` | Create | JSON schema with type + cross-field validation. |
| `charts/webhookd/README.md.gotmpl` | Create | Helm-docs source. |
| `charts/webhookd/README.md` | Create | Generated by helm-docs. |
| `charts/webhookd/ci/ci-values.yaml` | Create | `ct install` overrides. |
| `charts/webhookd/templates/_helpers.tpl` | Create | Named templates incl. `enabledProviders`. |
| `charts/webhookd/templates/NOTES.txt` | Create | Post-install hints. |
| `charts/webhookd/templates/deployment.yaml` | Create | Full Pod spec, env vars, probes. |
| `charts/webhookd/templates/service.yaml` | Create | Two named ports. |
| `charts/webhookd/templates/serviceaccount.yaml` | Create | SA in release ns (gated). |
| `charts/webhookd/templates/role.yaml` | Create | Role in target ns (cross-ns). |
| `charts/webhookd/templates/rolebinding.yaml` | Create | RoleBinding in target ns. |
| `charts/webhookd/templates/secret.yaml` | Create | Inline-create signing secret (gated). |
| `charts/webhookd/templates/servicemonitor.yaml` | Create | Prometheus Operator (gated). |
| `charts/webhookd/templates/networkpolicy.yaml` | Create | Default-deny + allow CIDRs (gated). |
| `charts/webhookd/templates/poddisruptionbudget.yaml` | Create | PDB (gated). |
| `charts/webhookd/templates/crd-precheck-job.yaml` | Create | Pre-install hook + SA + ClusterRole. |
| `charts/webhookd/tests/*_test.yaml` | Create | helm-unittest cases per template. |
| `.github/workflows/chart-ci.yml` | Create | Lint + unittest + ct + helm-docs drift. |
| `.github/workflows/chart-release.yml` | Create | OCI + gh-pages publishing. |
| `.github/workflows/helm-docs.yml` | Create | Drift check on values changes. |
| `.github/workflows/pr-labels.yml` | Modify | Add `chart` label rule. |
| `.github/renovate.json` | Create | appVersion tracking against goreleaser tags. |
| `README.md` | Modify | Helm-first install instructions. |
| `deploy/rbac/*.yaml` | Modify | Banner comment marking fixture-only. |
| `docs/runbook/release-checklist.md` | Create | Smoke-test commands captured. |
| `docs/design/0003-...md` | Modify | Status → Implemented at the end. |
| `CLAUDE.md` | Modify | Phase 3 entry + chart patterns. |

## Testing Plan

- **Helm template + lint** — `helm lint charts/webhookd` clean; `helm
  template charts/webhookd ...` produces parseable YAML for every
  combination of value toggles documented in DESIGN-0003.
- **helm-unittest** — at least one case per template, covering both
  the gated-on and gated-off paths plus the `required` failure modes
  for each conditionally-required value. Run via `make chart-test`
  and CI.
- **chart-testing** — `ct install` against kind in CI on every PR
  touching `charts/**`. CRD pre-applied from `deploy/crds/`.
  `ct.yaml` keeps lint config close to repo-guardian's defaults.
- **End-to-end smoke** — Phase 6 manual test plan against a fresh
  kind cluster for both OCI and gh-pages install paths. Captured in
  `docs/runbook/release-checklist.md` for repeatability.
- **Schema validation** — invalid values (`jsm.crIdentityProviderID=""`
  with `jsm.enabled=true`, `signing.existingSecret=""` with
  `signing.createSecret=false`, unknown top-level keys) all fail
  `helm install --dry-run` with schema errors.
- **Precheck hook behavior** — install against a cluster missing the
  CRD fails the pre-install hook with a clear error and applies
  no other manifests; install with `crdPrecheck.enabled=false`
  succeeds even without the CRD (then CrashLoops on first webhook,
  as documented).
- **Renovate dry-run** — first PR that lands renovate config also
  triggers a dependency-dashboard issue we can sanity-check.

## Dependencies

- **Helm 3.13+** — chart `apiVersion: v2` and OCI publishing both
  need 3.13 or newer. Pinning **3.19.0** exact in `mise.toml`
  (Resolved Decision §2 — exact patch versions across all helm
  tooling for reproducibility).
- **`samlgroupmappings.wiz.webhookd.io` CRD** — Phase 1 onward.
  Existing fixture at `deploy/crds/samlgroupmapping.yaml` from
  IMPL-0002 is the install-time source of truth.
- **goreleaser tag `v0.1.0`** — must be cut **before** Phase 0
  (Resolved Decision §1). Chart `appVersion: 0.1.0` references the
  resulting `ghcr.io/donaldgifford/webhookd:0.1.0` image. Renovate's
  `appVersion` tracking takes over for subsequent binary tags
  (`v0.1.1`, `v0.2.0`, …).
- **GitHub repo settings:**
  - Pages enabled with source `gh-pages` branch (chart-releaser-action
    needs this).
  - Packages permission for OCI push to ghcr.io (needs `packages:
    write` in workflow + first push must be from a maintainer to
    create the package; subsequent pushes work via `GITHUB_TOKEN`).
- **kind / chart-testing / helm-unittest** — pinned in `mise.toml`
  Phase 0; CI uses official actions for the same versions.

## Resolved Decisions

Drafted as Open Questions; resolved with the user 2026-04-28 before
Phase 0 starts. Cascading consequences from each decision are applied
to the body above in the same pass — no Resolved Decision contradicts
the phase tasks.

1. **First-release `appVersion`: cut `v0.1.0` binary first.** Chart
   `0.1.0` ships against `appVersion: 0.1.0` — same number on both
   sides. **Reasoning:** version skew at v0.0.x for the first release
   is the kind of subtle thing that breeds confusion later (which
   chart pinned which binary?), and it costs us a single binary tag
   to avoid it. The pre-1.0 divergence allowance from
   DESIGN-0003 §Resolved-Decision §8 stays in effect for *future*
   chart-only bumps; we just don't *start* with a divergence.
   **Cascading:** Phase 0 gains a "cut v0.1.0 binary" prerequisite
   task; Phase 6 first-step verifies the binary tag exists; the
   Dependencies section calls out the binary tag as a hard
   precondition.

2. **Helm tooling versions: exact patch pins.** `helm = 3.19.0`,
   `helm-cr = 1.8.1`, `helm-ct = 3.14.0`, `helm-diff = 3.15.0`,
   `helm-docs = 1.14.2`, `kubectl = 1.31.4`, helm-unittest plugin =
   `1.0.3`. **Reasoning:** the rest of `mise.toml` already pins exact
   patch versions (Go, golangci-lint, etc.) — consistency wins over
   "let mise pick latest." Renovate updates these via PRs same as the
   Go toolchain pins. Pin set was validated against the contributor's
   pre-existing global mise install.

3. **Helm tooling distribution: mirror repo-guardian.** mise pins the
   five helm-* binaries (`helm`, `helm-cr`, `helm-ct`, `helm-diff`,
   `helm-docs`); helm-unittest installs as a *helm plugin* via
   `make chart-tools`. **Reasoning:** repo-guardian's split is the
   path of least resistance — helm-unittest's upstream distribution
   *is* a helm plugin (no standalone binary), and mise doesn't
   manage helm plugins. The Makefile bootstrap target keeps the
   command discoverable. **Cascading:** Phase 0 mise pins updated;
   `make chart-tools` task added; mise install hooks call it so a
   fresh checkout doesn't pay the install cost twice.

4. **CRD-precheck image: Chainguard.** `cgr.dev/chainguard/kubectl:latest-dev`
   exposed via `crdPrecheck.image.{repository,tag}` for air-gapped
   override. **Reasoning:** distroless + signed + maintained;
   Bitnami's public-registry deprecation makes `bitnami/kubectl`
   risky for a year-out workload, and `registry.k8s.io/kubectl`
   isn't actually a thing (the registry hosts kubelet/kube-proxy,
   not a `kubectl` image). Chainguard is the same supply-chain
   posture we'd want once we adopt cosign/sigstore (Resolved
   Decision §9 follow-up).

5. **OCI path: `oci://ghcr.io/donaldgifford/charts/webhookd`** — the
   drafted form. **Reasoning:** the `/charts/` segment cleanly
   separates chart packages from binary images
   (`ghcr.io/donaldgifford/webhookd` would collide with the existing
   container image namespace), and the slight learning cost is one
   `--repo` flag in install instructions.

6. **`helm-docs.yml` failure mode: fail with "run `make chart-docs`."**
   **Reasoning:** option (a) — auto-commit bots need a PAT and add
   moving parts; the helm-docs invocation is identical to what
   contributors run via `make chart-docs`, so the failure message
   literally tells them the fix. We can graduate to auto-commit
   later if the friction proves real.

7. **`gh-pages` init: trust chart-releaser-action.** No pre-seeded
   placeholder branch. **Reasoning:** the action's `--gh-pages`
   handling does work the first time according to recent issues —
   if it doesn't, the failure mode is loud (release job red) and
   fixable with a 30-second manual orphan-branch creation. Not worth
   pre-empting.

8. **ghcr.io visibility: one-time manual flip.** First OCI push
   creates the package as private; toggle to public via the package
   settings page once. **Reasoning:** `gh api
   /user/packages/container/charts%2Fwebhookd/visibility` works but
   requires a PAT with `write:packages` scope (default
   `GITHUB_TOKEN` can't change package visibility). One-time manual
   click beats stashing a PAT for a single use. **Cascading:**
   Phase 5 release-workflow tasks lose the visibility-flip subtask;
   Phase 6 first-release gains an explicit one-time visibility-flip
   task.

9. **Sigstore / cosign: deferred.** Not in v0.1.0; tracked as
   follow-up. **Reasoning:** the chart's threat model is "verify the
   contents match what GitHub Actions produced." Sigstore would add
   that proof, but we don't sign the *binary* image either yet, so
   doing only the chart is asymmetric. Earn back the time later
   when we sign both.

10. **`pr-labels.yml`: path-based.** Rule fires on
    `paths-ignore: []`, `paths: [charts/**, ct.yaml,
    charts/.yamllint.yml]`. **Reasoning:** webhookd's existing
    pr-labels rules are all path-based, not branch-name-based. Stay
    consistent — branch-name heuristics rot the moment someone uses
    a non-conventional branch name.

11. **Release notes: per-chart `CHANGELOG.md`.** `charts/webhookd/CHANGELOG.md`
    is hand-written; the release workflow extracts the latest entry
    and feeds it to `chart-releaser-action`. **Reasoning:** the
    binary release uses git-cliff for *commit-derived* notes; the
    chart's audience cares about *user-visible* changes (values
    schema breaks, default flips, new gates) which don't always
    map to commit messages. A hand-curated CHANGELOG forces the
    author to think through the user-visible delta. **Cascading:**
    Phase 0 creates the file with a `## 0.1.0` seed entry; Phase 5
    release workflow gains a release-notes-extraction step;
    File Changes table adds the CHANGELOG row.

12. **Local `make chart-test` against kind.** Same runtime as CI.
    **Reasoning:** envtest's `apiextensions.k8s.io/v1` gap is a real
    blocker (the CRD-precheck Job needs the CRD API), and
    "fast locally / different in CI" creates exactly the kind of
    Heisenbug the Phase 4 e2e is meant to prevent. Use the same
    tool both places.

## References

- **Implements:** [DESIGN-0003](../design/0003-helm-chart-and-release-pipeline-for-webhookd.md).
- **Reference implementation:**
  [donaldgifford/repo-guardian](https://github.com/donaldgifford/repo-guardian) —
  source for chart layout, workflows, `ct.yaml`, `renovate.json`.
- **Helm tooling:**
  - [chart-releaser-action](https://github.com/helm/chart-releaser-action)
  - [chart-testing](https://github.com/helm/chart-testing) (`ct lint`, `ct install`)
  - [helm-unittest](https://github.com/helm-unittest/helm-unittest)
  - [helm-docs](https://github.com/norwoodj/helm-docs)
  - [Helm OCI registry support](https://helm.sh/docs/topics/registries/)
- **Webhookd context:**
  - DESIGN-0001 / IMPL-0001 — env-var matrix the chart maps to.
  - DESIGN-0002 / IMPL-0002 — RBAC verbs, target namespace, CRD
    name (`samlgroupmappings.wiz.webhookd.io`).
  - ADR-0007 — trace-id annotation; relevant when chart enables
    `config.tracing.*`.
