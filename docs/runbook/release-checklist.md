# webhookd chart release checklist

Captured during IMPL-0003 Phase 6. Re-run from the top for every chart
release; check off as you go.

> [!IMPORTANT]
> The chart's `appVersion` and the binary's git tag must match for
> the first release (Resolved Decision §1). Before triggering the
> release workflow, confirm a matching `vX.Y.Z` tag exists on
> `donaldgifford/webhookd` and that
> `ghcr.io/donaldgifford/webhookd:X.Y.Z` is published.

## Pre-flight

- [ ] Cut and merge a PR bumping `charts/webhookd/Chart.yaml`'s
      `version` and `appVersion` together. Add a `## X.Y.Z - YYYY-MM-DD`
      entry to `charts/webhookd/CHANGELOG.md` summarizing the
      user-visible delta.
- [ ] `make helm-test`, `make helm-ct-lint`, `make helm-docs-check`
      all pass on `main` post-merge.
- [ ] `actionlint .github/workflows/chart-release.yml` clean.
- [ ] Binary release `vX.Y.Z` exists at
      <https://github.com/donaldgifford/webhookd/releases/tag/vX.Y.Z>
      and `docker pull ghcr.io/donaldgifford/webhookd:X.Y.Z` succeeds.

## Release

- [ ] Trigger the release workflow:
      ```bash
      gh workflow run chart-release.yml \
        --ref main \
        --field chart-version=X.Y.Z
      ```
- [ ] Watch both jobs to green:
      ```bash
      gh run watch
      ```
- [ ] Verify the OCI artifact exists:
      ```bash
      helm pull oci://ghcr.io/donaldgifford/charts/webhookd \
        --version X.Y.Z --destination /tmp
      ```
- [ ] Verify the gh-pages release exists:
      <https://github.com/donaldgifford/webhookd/releases/tag/webhookd-X.Y.Z>

## One-time setup (only the very first release)

- [ ] **Flip the OCI package visibility to public.**
      Navigate to
      <https://github.com/users/donaldgifford/packages/container/charts%2Fwebhookd/settings>
      and change the visibility from `Private` → `Public`. Resolved
      Decision §8: one-time manual click beats stashing a PAT.
- [ ] Confirm anonymous `helm pull` works:
      ```bash
      docker logout ghcr.io
      helm pull oci://ghcr.io/donaldgifford/charts/webhookd --version X.Y.Z
      ```
- [ ] Confirm the gh-pages mirror is reachable and indexed:
      ```bash
      helm repo add webhookd https://donaldgifford.github.io/webhookd
      helm repo update
      helm search repo webhookd
      ```

## OCI smoke test (kind)

- [ ] ```bash
      kind create cluster --name webhookd-oci-smoke
      kubectl apply -f deploy/crds/samlgroupmapping.yaml
      kubectl create namespace webhookd
      kubectl create namespace wiz-operator
      ```
- [ ] ```bash
      helm install webhookd \
        oci://ghcr.io/donaldgifford/charts/webhookd \
        --version X.Y.Z \
        --namespace webhookd \
        --set jsm.crIdentityProviderID=smoke \
        --set signing.createSecret=true \
        --set signing.secret=smoketest
      ```
- [ ] Verify Pod ready:
      ```bash
      kubectl wait pod -l app.kubernetes.io/instance=webhookd \
        -n webhookd --for=condition=ready --timeout=60s
      ```
- [ ] Verify admin probes:
      ```bash
      kubectl port-forward -n webhookd svc/webhookd 9090:9090 &
      curl -fsS http://localhost:9090/healthz
      curl -fsS http://localhost:9090/readyz
      ```
- [ ] Tear down: `kind delete cluster --name webhookd-oci-smoke`.

## gh-pages smoke test (separate kind cluster)

- [ ] ```bash
      kind create cluster --name webhookd-pages-smoke
      kubectl apply -f deploy/crds/samlgroupmapping.yaml
      kubectl create namespace webhookd
      ```
- [ ] ```bash
      helm repo add webhookd https://donaldgifford.github.io/webhookd
      helm install webhookd webhookd/webhookd \
        --version X.Y.Z \
        --namespace webhookd \
        --set jsm.crIdentityProviderID=smoke \
        --set signing.createSecret=true \
        --set signing.secret=smoketest
      ```
- [ ] Same Pod-ready + admin-probe assertions as the OCI smoke test.
- [ ] Tear down: `kind delete cluster --name webhookd-pages-smoke`.

## Cleanup verification

- [ ] After both smoke tests, on a freshly recreated kind:
      ```bash
      helm uninstall webhookd -n webhookd
      ```
- [ ] Verify zero residue:
      ```bash
      kubectl get role,rolebinding -n wiz-operator
      kubectl get clusterrole,clusterrolebinding | grep webhookd-crd-precheck
      kubectl get sa -n webhookd | grep webhookd-crd-precheck
      ```
      All three should return no rows. The precheck hook resources
      are cleaned up by `helm.sh/hook-delete-policy:
      before-hook-creation,hook-succeeded`; the chart's own Role +
      RoleBinding land in `wiz-operator` and are removed by
      `helm uninstall` along with the rest of the release.

## Rollback

If the release workflow succeeded but the smoke tests fail:

- OCI: `oras manifest delete ghcr.io/donaldgifford/charts/webhookd:X.Y.Z`
  removes the bad version. Future installs fall back to the previous
  version.
- gh-pages: revert the gh-pages branch commit that the release added
  (`git revert <sha>` on the gh-pages branch). The next install
  resolves to the previous version.
- Either way: cut a new chart `X.Y.(Z+1)` with the fix; do not retry
  the same `X.Y.Z` against `chart-releaser-action` (it has
  `skip_existing: true`).

## See also

- [DESIGN-0003](../design/0003-helm-chart-and-release-pipeline-for-webhookd.md)
- [IMPL-0003](../impl/0003-helm-chart-and-release-pipeline-implementation.md)
  — Phase 6 success criteria reference this file.
- [chart-releaser-action](https://github.com/helm/chart-releaser-action)
- [Helm OCI registry support](https://helm.sh/docs/topics/registries/)
