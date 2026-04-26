# syntax=docker/dockerfile:1.7

# webhookd — multi-stage container build.
#
# Build stage: Alpine + Go for a small, static, cross-compilable build.
# Runtime stage: distroless static nonroot — no shell, no package manager,
# minimal attack surface. See IMPL-0001 §Resolved Decisions #7.
#
# Versioning: VERSION and COMMIT are passed in as build args from
# docker-bake.hcl and end up in the binary via -ldflags so
# `webhookd_build_info{version, commit}` exposes them at runtime.

# -----------------------------------------------------------------------------
# Build stage
# -----------------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.26.1-alpine AS build

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown

WORKDIR /src

# Cache module downloads independently of source so a code-only change
# does not bust the dep layer.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

# CGO disabled → static binary that runs on distroless static.
# -trimpath strips local paths (reproducible builds).
# -s -w drop debug + symbol tables to keep the image small; build provenance
# still flows through main.version / main.commit via the explicit -X flags.
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build \
        -trimpath \
        -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
        -o /out/webhookd \
        ./cmd/webhookd

# -----------------------------------------------------------------------------
# Runtime stage
# -----------------------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/webhookd /webhookd

# Public listener (webhook intake) and admin listener (metrics + probes).
# See DESIGN-0001 §HTTP Endpoints.
EXPOSE 8080 9090

USER nonroot:nonroot

ENTRYPOINT ["/webhookd"]
