# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project state

`webhookd` is a Go webhook receiver. IMPL-0001 Phases 0–2 are complete: bootstrap (`go.mod`, `Dockerfile`, `docker-bake.hcl`, `LICENSE`), configuration (`internal/config`, table-driven, 100% coverage), and the observability substrate (`internal/observability/{logging,tracing,metrics}.go`) — slog with `trace_id`/`span_id` correlation via a wrapper handler that re-implements `WithAttrs`/`WithGroup` so derived loggers keep the wrapper, OTel `TracerProvider` with OTLP/HTTP exporter and a `samplerFor` helper that maps ratios to `ParentBased` samplers, and a private Prometheus registry exposing every instrument from DESIGN-0001 §Metrics plus go/process collectors and `webhookd_build_info`. `make ci` is green. Active work tracked in `docs/impl/0001-phase-1-stateless-receiver-implementation.md`.

The substantive specs:

- `docs/design/0001-stateless-webhook-receiver-phase-1.md` — DESIGN-0001: Phase 1, the stateless HTTP receiver (routing, HMAC verification, OTel tracing, Prometheus metrics, slog with trace correlation, graceful shutdown, admin listener for metrics + probes). The substrate.
- `docs/design/0002-jsm-webhook-to-samlmapping-provisioning-phase-2.md` — DESIGN-0002: Phase 2, JSM webhook → `SAMLMapping` CR provisioning via controller-runtime SSA, with sync watch-and-respond.
- `docs/impl/0001-phase-1-stateless-receiver-implementation.md` — IMPL-0001: phased task list for landing DESIGN-0001. Resolved Decisions section captures answers to design questions (header names, canonical signing format, request ID generator, etc.) — read before implementing each phase.
- `docs/adr/0001` … `0006` — ADRs for the settled decisions: stdlib `net/http` routing, Prometheus+OTel signal split, env-only config, controller-runtime typed client, Server-Side Apply, synchronous response contract. Read these before arguing a different choice.
- `archive/walk1.md` and `archive/walk2.md` (gitignored) — line-by-line implementation walkthroughs that are prescriptive about package layout (`cmd/webhookd/main.go` for wiring only; `internal/{config,observability,httpx,webhook}` each with one reason to change) and the startup phases. **When implementing, follow these walkthroughs — they're the source of truth for structure.**

When starting implementation work, read the relevant design + walkthrough pair plus IMPL-0001's phase tasks first. Don't invent architecture — the decisions are already made and captured in ADRs.

## Repo provenance

Scaffolded from the `go-ext` blueprint in a `forge` registry (`.forge-lock.yaml`). Files listed there with `strategy: overwrite` are regenerated on `forge sync` and **should not be hand-edited** — change them upstream in the registry. That covers most dotfiles (`.golangci.yml`, `.goreleaser.yml`, `.github/workflows/*`, lint/format configs, `scripts/labels.sh`, etc.).

Scaffold cleanup is complete: `.goreleaser.yml` was rewritten to `webhookd`, `make run` now points at `$(BIN_DIR)/$(PROJECT_NAME)`, `goimports.local-prefixes` was changed to `github.com/donaldgifford/webhookd`, and the four root-level `design0001.md` / `design0002.md` / `walk1.md` / `walk2.md` files were moved to `archive/` (gitignored) once the docz-canonical copies landed under `docs/`.

## Commands

Use `make` — it's the canonical entrypoint. `make help` lists targets.

- `make build` — builds `build/bin/webhookd` with version ldflags
- `make test` — `go test -v -race ./...`
- `make test-pkg PKG=./internal/webhook` — single package with race detector
- `make test-coverage` — writes `coverage.out`; `make test-report` opens HTML
- `make lint` / `make lint-fix` — golangci-lint (v2.11.4, configured against Uber's Go Style Guide; `fmt` also runs gofumpt, gci, golines)
- `make fmt` — gofmt + goimports with `-local github.com/donaldgifford`
- `make check` — quick pre-commit: lint + test
- `make ci` — full local CI: lint + test + build + license-check
- `make license-check` — allowlist: Apache-2.0, MIT, BSD-2-Clause, BSD-3-Clause, ISC, MPL-2.0
- `make release-check` / `make release-local` — goreleaser validation / snapshot
- `make run-local` — builds then runs `$(BIN_DIR)/webhookd`
- `docker buildx bake` — builds `webhookd:dev` for the local platform via `docker-bake.hcl`. CI uses `docker buildx bake ci` for the multi-arch (linux/amd64 + linux/arm64) target.

## Toolchain

Tool versions are pinned in `mise.toml`. Use `mise install` to materialize them. Notable pins: Go 1.26.1, golangci-lint 2.11.4, markdownlint-cli2, yamlfmt/yamllint, prettier, checkmake, git-cliff, syft, govulncheck, go-licenses, goimports, mockery, godoc, cobra-cli. Also `docz` (from `github:donaldgifford/docz`) for managing docs.

## Lint configuration quirks

`.golangci.yml` is strict (Uber-style) and enables `revive`, `gocyclo` (complexity 15), `gocognit` (30), `funlen` (100 lines / 50 stmts), `nestif` (4), plus full `staticcheck`/`gosec`/`errcheck` including blank-identifier and type-assertion checks. Test files (`_test.go`) and `mock_*.go` files have relaxations — don't add nolint directives on test code for rules already waived there.

The `goheader` linter is enabled but not yet configured with a template; it's a no-op until Phase 6 wires `licenses-header.txt` and the `goheader.values` block. Don't add per-file SPDX headers to new Go files until then — they'd just have to move when the template lands.

## Documentation workflow

`docz` is the documentation CLI. Config is `.docz.yaml`; docs live in `docs/{adr,rfc,design,impl,plan,investigation}`. When adding a new design/decision doc, prefer `docz create <type>` (via the `docz:create` skill) over hand-rolling — it auto-updates the README index table. Run `docz update` after editing frontmatter to refresh indexes.
