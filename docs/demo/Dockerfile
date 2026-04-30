# syntax=docker/dockerfile:1.7

# webhookd-demo container image.
# Builds two distroless runtime images from a shared builder: webhookd-demo
# itself and the mock-operator helper.

# --- builder stage ---
FROM golang:1.26-alpine AS builder

ARG VERSION=dev
ARG COMMIT=unknown
ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

# Cache deps.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Source.
COPY . .

# Build webhookd-demo.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build \
      -trimpath \
      -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o /out/webhookd-demo \
      ./cmd/webhookd-demo

# --- mock-operator stage ---
FROM builder AS mock-operator-builder
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build \
      -trimpath \
      -ldflags "-s -w" \
      -o /out/mock-operator \
      ./cmd/mock-operator

# --- runtime: webhookd-demo ---
FROM gcr.io/distroless/static-debian12:nonroot AS webhookd-demo
COPY --from=builder /out/webhookd-demo /webhookd-demo
EXPOSE 8080 9090
USER nonroot:nonroot
ENTRYPOINT ["/webhookd-demo"]

# --- runtime: mock-operator ---
FROM gcr.io/distroless/static-debian12:nonroot AS mock-operator
COPY --from=mock-operator-builder /out/mock-operator /mock-operator
USER nonroot:nonroot
ENTRYPOINT ["/mock-operator"]
