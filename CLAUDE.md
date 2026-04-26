# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project state

`webhookd` is a Go webhook receiver. **IMPL-0001 is complete** — all six phases shipped: bootstrap, configuration (`internal/config`), observability (`internal/observability`), HTTP framework (`internal/httpx` — middleware, admin mux with pprof gated by `WEBHOOK_PPROF_ENABLED`, server constructor, per-provider token-bucket rate limiting via `golang.org/x/time/rate`), webhook handler with HMAC-SHA256 + replay protection (`internal/webhook`), full application wiring (`cmd/webhookd/main.go`), and SPDX-style license headers across every Go file (`licenses-header.txt` + `goheader` lint). `make ci` is green. The next initiative is Phase 2 (DESIGN-0002: JSM webhook → SAMLMapping CR provisioning); a new IMPL doc for that scope should land before code begins.

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

`goheader` is configured against `licenses-header.txt`. Every new Go file needs the SPDX two-line header — `make fmt` won't add it for you. Copy the header from any existing file.

Recurring lint gotchas in this repo (all hit during IMPL-0001):

- **`errcheck` with `check-blank: true`** flags `_, _ = w.Write(...)` — you can't just blank-discard. Either handle the error properly or factor into a helper that documents *why* discarding is safe.
- **`gocritic hugeParam`** flags large value receivers. For interface-fixed signatures (`slog.Handler.Handle` takes `slog.Record` by value), use `//nolint:gocritic` with a reason. For our own constructors that take `*Config` by value at the boundary (e.g., `webhook.NewHandler(HandlerConfig, …)`), the same nolint applies — by-value is intentional so callers can build literals.
- **`noctx`** rejects `httptest.NewRequest`, `http.Get`, `http.Post` in tests. Always use `httptest.NewRequestWithContext` and `http.NewRequestWithContext` + `http.DefaultClient.Do`.
- **`contextcheck`** flags shutdown paths that intentionally use `context.WithTimeout(context.Background(), …)` because the parent ctx is signal-cancelled. The cleanest pattern: extract a named helper (`drainServers`, `shutdownTracerProvider`) and put `//nolint:contextcheck` at the call site with the reason.
- **`revive context-as-argument`** — `context.Context` must be the **first** parameter. `httpx.NewServer(ctx, addr, h, cfg)` is correct; `(addr, h, cfg, ctx)` will fail lint.
- **`nakedret`** triggers when named returns + a bare `return` span more than ~5 lines. Just write explicit returns.
- **`gocritic emptyStringTest`** prefers `s == ""` over `len(s) == 0`.
- **`gocritic httpNoBody`** prefers `http.NoBody` over `nil` for empty request bodies.
- **`gocritic exitAfterDefer`** flags `os.Exit` after `defer cancel()`. Wrap the body in a `realMain() int` helper and `os.Exit(realMain())` so deferred cleanup runs.

## Testing patterns that have bitten us

- **Vec metrics with no children render zero lines** on a Prometheus scrape. Tests asserting "metric `foo` is in the exposition" must first trigger at least one observation on the labeled metric, otherwise the test fails despite registration being correct.
- **`hex.DecodeString` is case-insensitive.** A signature test that compares received-header strings byte-for-byte is wrong. The fuzz target `internal/webhook.FuzzSignatureVerify` caught this — compare decoded byte slices, not raw hex strings.
- **`r.PathValue("provider")` is empty in pre-mux middleware.** Go 1.22+ `ServeMux` only populates path values *after* routing. Middleware in the outer chain (e.g., `httpx.RateLimit`) must parse the URL path manually. We use `providerFromPath("/webhook/<provider>")` exactly for this.
- **OTel `BatchSpanProcessor` leaks goroutines.** `goleak.VerifyTestMain` will fail unless either (a) `tp.Shutdown(ctx)` is awaited before the test exits, or (b) `goleak.IgnoreTopFunction("...batchSpanProcessor.processQueue")` is passed. The integration test in `cmd/webhookd/main_test.go` uses option (b).
- **Shutdown contexts must be detached.** `context.WithTimeout(context.Background(), cfg.ShutdownTimeout)` — never derived from the run-loop ctx — so the drain budget survives a SIGTERM-cancelled parent.

## Architectural patterns

- **Narrow config structs at package boundaries.** Each `internal/` package takes the minimum it needs (`webhook.HandlerConfig`, `httpx.AdminConfig`, `httpx.RateLimitConfig`) — never the full `*config.Config`. Keeps coupling tight and tests easy to fake.
- **Per-key lazy resources via `sync.Map`.** The rate limiter's per-provider `*rate.Limiter` is the canonical example: `Load` first, `LoadOrStore` on miss, no GC of entries (provider set is bounded by Phase 2's allow-list).
- **Single private Prometheus registry.** `observability.NewMetrics` returns a fresh `*prometheus.Registry` every call — no `DefaultRegisterer` — so tests can spin up isolated harnesses without leaking state. The constructor takes `*config.Config` only because `BuildInfo` lives there.

## Smoke testing the binary

Until a docker-compose dev stack lands, the quickest smoke test is `docker buildx bake webhookd-local` followed by `docker run -p 8080:8080 -p 9090:9090 -e WEBHOOK_SIGNING_SECRET=topsecret -e WEBHOOK_TRACING_ENABLED=false webhookd:dev`, then curl the admin endpoints and post a signed payload (see the README quick-start for the exact `openssl dgst` command). Disabling tracing avoids a hanging exporter when no OTLP collector is reachable.

## Documentation workflow

`docz` is the documentation CLI. Config is `.docz.yaml`; docs live in `docs/{adr,rfc,design,impl,plan,investigation}`. When adding a new design/decision doc, prefer `docz create <type>` (via the `docz:create` skill) over hand-rolling — it auto-updates the README index table. Run `docz update` after editing frontmatter to refresh indexes.
