---
id: ADR-0001
title: "Use stdlib net/http ServeMux for HTTP routing"
status: Accepted
author: Donald Gifford
created: 2026-04-24
---
<!-- markdownlint-disable-file MD025 MD041 -->

# 0001. Use stdlib net/http ServeMux for HTTP routing

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

webhookd sits on the edge of the network, accepting signed deliveries from
external providers. Every dependency adds a supply-chain surface, a versioning
schedule we have to track, and an upgrade cost when Go's standard library
evolves under it. Historically, Go services reached for chi, gorilla/mux,
gin, or echo because `net/http.ServeMux` did not support method matching or
path parameters.

Go 1.22 added method-and-pattern routing to the stdlib `ServeMux`
(`"POST /webhook/{provider}"`, `r.PathValue("provider")`). Go 1.21 added
`log/slog` to the stdlib, eliminating the other usual reason to pull in a
router-plus-logger bundle (zerolog/logrus). Together, the baseline of what
we would have reached for a third-party router to provide is now in the
stdlib.

## Decision

Use `net/http.ServeMux` (Go 1.22+) for all HTTP routing in webhookd. Do not
import chi, gorilla/mux, gin, echo, fiber, or any other router.

Route patterns follow the `"METHOD /path/{param}"` convention. Cross-cutting
concerns (recover, tracing, request-ID, logging, metrics) are implemented as
`func(http.Handler) http.Handler` middleware composed by a small in-house
`httpx.Chain` helper.

## Consequences

### Positive

- Fewer third-party dependencies to vet, upgrade, and track CVEs for.
- One mental model for anyone reading the code — stdlib docs are the docs.
- Middleware is just `func(http.Handler) http.Handler`, which is what every
  Go HTTP library eventually compiles to anyway.
- The Prometheus `route` label can use `http.Request.Pattern` directly
  (Go 1.22+), giving us the ServeMux pattern without a separate lookup, which
  keeps metric cardinality bounded without router-specific glue.

### Negative

- No built-in middleware library — we write the handful of middleware we need
  (`Recover`, `RequestID`, `SLog`, `Metrics`) ourselves. Each is small and
  testable, but it is code we own.
- Less expressive routing than chi: no route groups, no sub-routers, no
  compiled regex patterns. We do not currently need any of these.
- Raises the minimum Go version to 1.22 (we target 1.23+ anyway).

### Neutral

- If routing needs ever outgrow `ServeMux` (e.g. we want regex-constrained
  path params, or route-specific middleware stacks), the migration to chi is
  mechanical because our handlers are plain `http.Handler`.

## Alternatives Considered

- **chi.** The de-facto idiomatic alternative. Would give us route groups and
  a mature middleware ecosystem. Rejected because the stdlib now covers our
  actual needs and chi's extra surface area is not free to carry.
- **gorilla/mux.** Was unmaintained for a while; now revived but still a
  heavier router than we need.
- **gin / echo / fiber.** Each imposes a framework shape (custom context
  types, their own middleware signatures). Rejected as overkill for a service
  whose HTTP surface is one POST route plus three admin endpoints.

## References

- DESIGN-0001 §Background — "stdlib-forward baseline" rationale.
- Go 1.22 release notes — enhanced `ServeMux` patterns:
  <https://go.dev/doc/go1.22#enhanced_routing_patterns>
- `log/slog` (Go 1.21+): <https://pkg.go.dev/log/slog>
