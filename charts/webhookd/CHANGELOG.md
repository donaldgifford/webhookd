# Changelog

All notable changes to the `webhookd` chart are documented in this file.
The release pipeline (Phase 5 of IMPL-0003) extracts the latest entry and
feeds it to `chart-releaser-action` as the GitHub release notes.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this chart adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## 0.1.0 - 2026-04-28

### Added

- Initial chart release tracking webhookd `v0.1.0`.
- Deployment, Service, ServiceAccount, Role, RoleBinding, Secret templates
  produce a complete deploy parity with `deploy/rbac/`.
- Optional ServiceMonitor, NetworkPolicy, and PodDisruptionBudget gated
  behind their respective `enabled` flags (default off).
- CRD-precheck `pre-install` / `pre-upgrade` hook (default on) that fails
  fast if `samlgroupmappings.wiz.webhookd.io` is missing.
- Cross-namespace RBAC: Role + RoleBinding land in `rbac.targetNamespace`
  while the ServiceAccount stays in the release namespace.
- Flat per-provider values block (`jsm.*`) with required-field
  cross-validation in `values.schema.json`.
- OCI distribution at `oci://ghcr.io/donaldgifford/charts/webhookd` plus
  classic Helm repository at `https://donaldgifford.github.io/webhookd`.
