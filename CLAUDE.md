# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project state

`webhookd` is a Go webhook receiver. The repository is currently a **scaffold**: `cmd/webhookd/main.go` contains only a package declaration, there is no `go.mod` yet, and no `internal/` packages exist. The substantive specs for what to build live in `docs/`:

- `docs/design/0001-stateless-webhook-receiver-phase-1.md` — DESIGN-0001: Phase 1, the stateless HTTP receiver (routing, HMAC verification, OTel tracing, Prometheus metrics, slog with trace correlation, graceful shutdown, admin listener for metrics + probes). The substrate.
- `docs/design/0002-jsm-webhook-to-samlmapping-provisioning-phase-2.md` — DESIGN-0002: Phase 2, JSM webhook → `SAMLMapping` CR provisioning via controller-runtime SSA, with sync watch-and-respond.
- `docs/adr/0001` … `0006` — ADRs for the settled decisions: stdlib `net/http` routing, Prometheus+OTel signal split, env-only config, controller-runtime typed client, Server-Side Apply, synchronous response contract. Read these before arguing a different choice.
- `walk1.md`, `walk2.md` (repo root) — line-by-line implementation walkthroughs that are prescriptive about package layout (`cmd/webhookd/main.go` for wiring only; `internal/{config,observability,httpx,webhook}` each with one reason to change) and the startup phases. **When implementing, follow these walkthroughs — they're the source of truth for structure.** Haven't been migrated into `docs/impl/` yet.

When starting implementation work, read the relevant design + walkthrough pair first. Don't invent architecture — the decisions are already made and captured in ADRs.

## Repo provenance

Scaffolded from the `go-ext` blueprint in a `forge` registry (`.forge-lock.yaml`). Files listed there with `strategy: overwrite` are regenerated on `forge sync` and **should not be hand-edited** — change them upstream in the registry. That covers most dotfiles (`.golangci.yml`, `.goreleaser.yml`, `.github/workflows/*`, lint/format configs, `scripts/labels.sh`, etc.).

One stale artifact to know about: `.goreleaser.yml` still references `forge` (binary name, `main: ./cmd/forge`, release owner/name). It needs rewriting to `webhookd` before the first release.

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

Note: `make run` currently points at `./build/bin/repo-guardian` (Makefile bug — another stale scaffold reference).

## Toolchain

Tool versions are pinned in `mise.toml`. Use `mise install` to materialize them. Notable pins: Go 1.26.1, golangci-lint 2.11.4, markdownlint-cli2, yamlfmt/yamllint, prettier, checkmake, git-cliff, syft, govulncheck, go-licenses, goimports, mockery, godoc, cobra-cli. Also `docz` (from `github:donaldgifford/docz`) for managing docs.

## Lint configuration quirks

`.golangci.yml` is strict (Uber-style) and enables `revive`, `gocyclo` (complexity 15), `gocognit` (30), `funlen` (100 lines / 50 stmts), `nestif` (4), plus full `staticcheck`/`gosec`/`errcheck` including blank-identifier and type-assertion checks. Test files (`_test.go`) and `mock_*.go` files have relaxations — don't add nolint directives on test code for rules already waived there.

One goimports footgun: the `local-prefixes` is `github.com/donaldgifford/keycloak-cli` (copy-paste from template). Imports in this repo will be grouped under `github.com/donaldgifford` via the `gci` `prefix(github.com/donaldgifford)` section, but if you care about import ordering precision, fix the `goimports.local-prefixes` to `github.com/donaldgifford/webhookd`.

## Documentation workflow

`docz` is the documentation CLI. Config is `.docz.yaml`; docs live in `docs/{adr,rfc,design,impl,plan,investigation}`. When adding a new design/decision doc, prefer `docz create <type>` (via the `docz:create` skill) over hand-rolling — it auto-updates the README index table. Run `docz update` after editing frontmatter to refresh indexes. The root-level `walk1.md` / `walk2.md` files still need to migrate into `docs/impl/`; `design0001.md` / `design0002.md` are now outdated copies of the canonical `docs/design/` versions.
