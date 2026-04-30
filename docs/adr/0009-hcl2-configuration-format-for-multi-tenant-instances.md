---
id: ADR-0009
title: "HCL2 configuration format for multi-tenant instances"
status: Proposed
author: Donald Gifford
created: 2026-04-30
---
<!-- markdownlint-disable-file MD025 MD041 -->

# 0009. HCL2 configuration format for multi-tenant instances

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

Proposed (extracted from RFC-0001). Supersedes ADR-0003 (environment-variable-only configuration) when RFC-0001 lands.

> **History:** an earlier draft of this ADR proposed YAML for v1 with HCL2 deferred. During DESIGN-0004 review, HCL2's typed-decoding-plus-validation collapsed the per-integration JSON-Schema fragmentation problem the YAML proposal would have introduced (see §Alternatives), and the user's earlier preference for HCL2 became the better-justified choice. The decision flipped before either was implemented.

## Context

ADR-0003 chose env-var-only configuration for Phase 1. That worked while webhookd was a single-tenant service; it doesn't scale to multi-tenant routing (multiple JSM tenants pointing at independent K8s clusters / AWS regions / etc.) without devolving into `WEBHOOK_INSTANCE_0_PROVIDER=jsm`-style nesting that's just a structured format pretending to be env vars.

RFC-0001 proposes a multi-tenant Provider × Backend architecture: a list of webhook instances, each binding one Provider to one Backend with their respective configurations, routed via `/{provider_type}/{webhook_id}`. That shape needs a structured config format with three properties:

1. **Per-integration typed schemas.** Each Provider and Backend has its own config struct, validation rules, and required-fields. A central JSON Schema would have to compose fragments from each integration package, leading to fragmentation (which file owns the schema?), drift (the Go struct and the schema diverge), and ceremony (every new integration ships two artifacts).
2. **First-class block-style structure.** The natural shape of webhookd config is `instance "id" { provider "type" { ... } backend "type" { ... } }` — polymorphic blocks where the block label drives the typed decoder. YAML expresses this awkwardly (`type:` discriminator field + manual lookup); HCL2 expresses it natively.
3. **Multi-file/directory loading.** Operators want to break the config across files (one `instance.hcl` per webhook, a `runtime.hcl` for cross-cutting settings) without webhookd doing its own glob-and-merge logic. HCL2 has this built in.

Four formats were evaluated:

- **Env vars (status quo).** Doesn't extend cleanly past one instance per provider type.
- **YAML config file.** Familiar; no new deps. But: per-integration schemas must be composed manually (each integration ships a JSON Schema fragment, the host registers and merges them at startup); polymorphic blocks need a `type:` discriminator field; multi-file loading is DIY.
- **HCL2 config file (or directory).** Each Provider/Backend ships a typed Go struct with `hcl:""` tags; `gohcl.DecodeBody` validates and decodes in one step. Polymorphic block-by-label is the native idiom (Terraform's `resource "aws_instance" { ... }`). Multi-file directory load via `hclparse.Parser` is standard. Adds one dep (`github.com/hashicorp/hcl/v2`, MPL-2.0, well-maintained).
- **CRDs (Kubernetes-native).** Declarative, GitOps-native, RBAC-able per-instance, free admission validation. Ties webhookd to running on Kubernetes — currently only the K8s *backend* depends on K8s. Hot-reload becomes operator-shaped.

## Decision

Use **HCL2** for the v1 multi-tenant config format. The chart-rendered `ConfigMap` carries either a single `webhookd.hcl` file or a directory of `.hcl` files (per operator preference); HCL2's parser merges them at load time.

```hcl
# webhookd.hcl — read at startup. Hot-reload not supported in v1.

defaults {
  idempotency_ttl = "5m"
  max_body_bytes  = 1048576
}

runtime {
  addr             = ":8080"
  admin_addr       = ":9090"
  shutdown_timeout = "30s"

  rate_limit {
    rps   = 50
    burst = 100
  }

  tracing {
    enabled  = true
    endpoint = "otel-collector.observability:4317"
  }
}

instance "abc123def456" {
  provider "jsm" {
    trigger_status = "Approved"

    fields {
      provider_group_id = "customfield_10001"
      role              = "customfield_10002"
      project           = "customfield_10003"
    }

    signing {
      secret_env       = "WEBHOOK_JSM_TENANT_A_SECRET"   # always env, never inline
      signature_header = "X-Hub-Signature-256"
      timestamp_header = "X-Webhook-Timestamp"
      skew             = "5m"
    }
  }

  backend "k8s" {
    kubeconfig_env       = "KUBECONFIG_TENANT_A"  # empty = in-cluster
    namespace            = "wiz-operator"
    identity_provider_id = "tenant-a-idp"
    sync_timeout         = "20s"
  }
}

instance "7xkqp3l9zwer" {
  provider "github" {
    events = ["pull_request", "check_run"]
    signing {
      secret_env = "WEBHOOK_GH_ORG_X_SECRET"
    }
  }

  backend "aws" {
    region    = "us-west-2"
    event_bus = "prod-events"
  }
}
```

Each integration ships its typed config struct alongside its `Provider` / `Backend` implementation:

```go
// internal/integrations/jsm/config.go
type Config struct {
    TriggerStatus string        `hcl:"trigger_status"`
    Fields        FieldsConfig  `hcl:"fields,block"`
    Signing       SigningConfig `hcl:"signing,block"`
}
```

The dispatcher's loader is one call per block: `gohcl.DecodeBody(providerBlock.Body, evalCtx, &jsm.Config{})` — validation (required fields, type errors, unknown keys) and decoding happen in one step. **No separate JSON Schema files; no manual schema registration; the Go struct + HCL tags *are* the schema.**

Secrets are *always* referenced by env-var name (`secret_env`), never inline. The chart loads them via `existingSecret` mappings, mirroring the pattern from DESIGN-0003.

Hot-reload is not in scope for v1. Configuration is read at startup; changes require a pod restart. Defer hot-reload to a follow-up RFC if operational pain shows up.

CRDs remain a clean upgrade for the day GitOps-per-instance management becomes load-bearing — the in-memory `Config` struct is independent of the wire format, so a CRD-watch-and-decode path is a contained future change.

## Consequences

### Positive

- **One source of truth per integration.** The Go struct + `hcl:""` tags are the schema. No per-integration JSON Schema fragment to register and merge; no schema-vs-struct drift.
- **Polymorphic block-by-label is native.** `provider "jsm" { ... }` keys directly to a registered Provider that exposes a typed `Config`. The dispatcher's loader doesn't need a `type:` discriminator field or a manual lookup-then-decode dance.
- **Multi-file directory loading is built in.** Operators can split their config across files (`instance-tenant-a.hcl`, `instance-tenant-b.hcl`, `runtime.hcl`) and point webhookd at the directory; HCL2 merges them at parse time. Helm chart generation can produce one file per instance template, which keeps `helm diff` outputs surgical.
- **Type-safe end to end.** `gohcl.DecodeBody` populates Go structs directly. Compile-time guarantees on the integration side; runtime errors at decode time with HCL2's structured diagnostics ("Required attribute not set: line 12, col 5").
- **Familiar to anyone who's written Terraform / Packer / Vault config.** The block syntax is well-understood in this part of the ecosystem.
- **Aligns with the existing chart pattern for sensitive values** (env-mapped via `existingSecret`); no inline credentials.

### Negative

- **One new Go dep.** `github.com/hashicorp/hcl/v2` (MPL-2.0, ~30k stars, actively maintained by HashiCorp, on the project's license allowlist via the existing MPL-2.0 entry). Stable surface; we pin a specific version in `go.mod`.
- **HCL2 is less common in cloud-native ecosystems than YAML.** Most operators write YAML; HCL2 is mostly Terraform / Packer / Vault / Consul. Contributors will have a small learning curve; the doc/runbook needs an HCL2 primer.
- **Tooling around HCL2 is less ubiquitous than YAML.** No `helmfile`-style rendering, fewer linters, no `yq`-equivalent in the standard toolchain. `hclfmt` (vendored from Terraform) handles formatting; we'd ship it as a dev tool.
- **HCL2's expression language is a power-user surface.** `${var.foo}`-style interpolations and built-in functions can produce config that's hard to reason about if abused. Mitigate by recommending plain block style for instance config; reserve interpolations for the rare case where shared values reduce duplication meaningfully.
- **Helm chart rendering produces HCL output, not YAML.** This is mostly fine — `ConfigMap.data` accepts arbitrary strings — but `helm diff` output for HCL config is less readable than YAML. Acceptable tradeoff.

### Neutral

- The `Config` struct is owned by `internal/config/`; the HCL2 decoder is the only thing that touches the file format. Tests target the struct, not the decoder, so swapping decoders later doesn't invalidate them.
- Hot-reload defers cleanly: when it lands, it'll be a `fsnotify`-backed reload of the same file/directory, swapping the in-memory instance map atomically.
- Helm chart values stay YAML (Helm's own format); the chart renders HCL into the `ConfigMap.data`. Two formats in the deployment pipeline; clean boundary.

## Alternatives Considered

- **Environment variables only (status quo, ADR-0003).** Doesn't scale past single-tenant. RFC-0001 supersedes ADR-0003 when this lands.
- **YAML config file** *(was the earlier proposal in this ADR).* Familiar; zero learning curve; no new dep. But each integration's per-block schema needs a JSON Schema fragment registered at startup, and polymorphic block-by-label requires a `type:` discriminator + manual lookup-then-decode pattern. Reasonable for a single-integration shape; pays a tax that grows linearly with the integration count. Reasons it lost to HCL2 in this revision:
  - **Schema fragmentation.** Each Provider/Backend would ship a JSON Schema fragment that the host composes at startup. Two artifacts per integration (Go struct + JSON Schema file), with constant drift risk.
  - **No native polymorphic blocks.** YAML's typed-discriminator pattern (`type: jsm` keying to a registered decoder) requires a manual two-pass parse: first decode `{ type, ... }`, then look up the typed decoder, then re-decode the rest. HCL2 does this in one call.
  - **No native multi-file loading.** Splitting config across `instance-*.yaml` files requires the host to glob-and-merge, with order semantics we'd have to define and document.
  - The "wire format independent of in-memory struct" framing was true of both, so the swap remained low-cost — just no longer needed.
- **CRDs (e.g. `Webhook` CRD).** GitOps-native; per-instance RBAC via Kubernetes; free admission validation via the schema. Forces K8s as a substrate dependency (today only the K8s backend needs K8s); hot-reload becomes operator-shaped (controller-runtime watch). Deferred — revisit when GitOps-per-instance becomes load-bearing.
- **TOML / JSON config.** Functional but no operational advantage. JSON in particular is awkward for human editing (no comments, strict syntax). Rejected as not pulling weight.

## References

- RFC-0001 — Generalize webhookd to Provider × Backend with Multi-Tenant Routing (the parent proposal that surfaces this decision).
- ADR-0003 — Environment-variable-only configuration (superseded by this ADR when RFC-0001 lands).
- DESIGN-0003 — Helm chart and release pipeline (the chart pattern this config integrates with).
- DESIGN-0004 — Multi-Tenant Provider × Backend Architecture (the design that consumes this decision).
- INV-0001 §Config: HCL2 vs YAML vs CRDs vs env (the original tradeoff analysis; pre-flip).
- [HCL2 specification](https://github.com/hashicorp/hcl/blob/main/hclsyntax/spec.md) — language reference.
- [`gohcl` package](https://pkg.go.dev/github.com/hashicorp/hcl/v2/gohcl) — the typed-decoding API the dispatcher will use.
- [Terraform's `resource "type" "name" { ... }` block-by-label pattern](https://developer.hashicorp.com/terraform/language/resources/syntax) — the precedent for `instance "id" { provider "type" { ... } }` shape.
