---
id: ADR-0003
title: "Environment-variable-only configuration"
status: Accepted
author: Donald Gifford
created: 2026-04-24
---

<!-- markdownlint-disable-file MD025 MD041 -->

# 0003. Environment-variable-only configuration

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

Accepted

## Context

Services commonly accept configuration through several channels at once: a
config file (`config.yaml`), command-line flags, and environment variables, with
a defined precedence between them. This flexibility is useful for desktop
tooling but expensive for a single-purpose server that runs in Kubernetes.

Each extra channel adds:

- Parsing code and its tests.
- A precedence story that has to be documented and understood.
- A second "which value actually won" debugging vector in production.
- Hot-reload expectations users may then form (reload file on SIGHUP, etc.).

webhookd's deployment model is a Kubernetes Deployment. Kubernetes already
expresses configuration as environment variables (from `envFrom: configMapRef`
and `envFrom: secretRef`), and Secret rotation via redeploy is the Helm-chart
native path. The OpenTelemetry SDK itself is configured through env vars
(`OTEL_*`). There is no value in routing that configuration through a file we
then parse in Go.

## Decision

All webhookd configuration comes from environment variables, parsed once at
startup by `internal/config.Load()`. There are no CLI flags, no config files,
and no runtime reload (no SIGHUP handler, no config-watch goroutine).

Namespaces:

- `WEBHOOK_*` — everything specific to webhookd.
- `WEBHOOK_<PROVIDER>_*` — provider-specific config (e.g. `WEBHOOK_JSM_*`).
- `OTEL_*` — standard OpenTelemetry SDK variables, read directly by the OTel
  SDK, not re-parsed by our code.

Parse errors are fatal; the process exits non-zero with a clear message on
stderr. There are no partial boots.

Configuration changes require a pod restart. Secret rotation is a redeploy.

## Consequences

### Positive

- One code path for config loading, ~100 lines of stdlib Go (`os.Getenv` + small
  typed helpers). No `viper`, no `envconfig`, no framework.
- No precedence story to explain or debug — the value came from the env.
- The OTel SDK's native `OTEL_*` vars are reused directly, so the tracing config
  surface is not duplicated.
- Natural fit with Kubernetes `envFrom` from ConfigMap and Secret refs, and with
  the External Secrets / Vault side-car pattern later.
- Immutable config per pod lifetime: a running pod's config cannot drift out
  from under you.

### Negative

- Every config change — including secret rotation — requires a pod restart. For
  webhookd this is intentional (replicas are interchangeable; rollouts are
  cheap) but worth stating.
- No introspection endpoint for "what config is currently loaded" beyond what we
  choose to log at startup. Operators who want to see config have to look at pod
  env or startup logs.
- Local development runs have to set env vars rather than edit a file. A
  `.envrc` / `direnv` workflow is the usual answer.

### Neutral

- If a future requirement actually needs hot reload (e.g. feature flags), that's
  a separate subsystem to add, not a change to how static config loads.
  Feature-flag SDKs already handle this out-of-band.

## Alternatives Considered

- **viper.** Would give us file + env + flag with precedence. Rejected: large
  dependency, more config surface than we need, and the file/flag paths would be
  dead code for every real deployment.
- **envconfig (sethvargo/kelseyhightower).** Smaller than viper and struct-tag
  driven. Rejected to keep the config package to stdlib only — the hand-written
  version is not meaningfully longer.
- **Config file only, mounted from ConfigMap.** Reasonable, but splits the
  config surface between file values and the OTel SDK's env vars that we still
  have to propagate. Merged env-only is simpler.
- **Flags plus env.** Standard stdlib `flag` package would work, but adds a
  second channel for every option and an ordering question at startup (flags
  override env, or env overrides flags?).

## References

- DESIGN-0001 §Configuration (env vars).
- OTel environment variable specification:
  <https://opentelemetry.io/docs/specs/otel/configuration/sdk-environment-variables/>
- The Twelve-Factor App, "Config": <https://12factor.net/config>
