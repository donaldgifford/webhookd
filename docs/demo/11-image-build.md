# 11. Image Build (docker buildx bake)

Production webhookd builds container images via `docker buildx bake`.
The demo follows the same pattern: a `docker-bake.hcl` file with one
target per build flavor, fed by a multi-stage `Dockerfile`.

## Files

```
docs/demo/
├── Dockerfile           # multi-stage build: builder → distroless
└── docker-bake.hcl      # bake targets
```

## Dockerfile

Two stages: a builder stage that compiles the binary statically, and
a distroless runtime that ships only the binary + CA certs.

### `Dockerfile`

```dockerfile
# syntax=docker/dockerfile:1.7

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

# Build the demo binary.
# CGO_ENABLED=0 + static linking lets us copy into a scratch/distroless
# image without a libc.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build \
      -trimpath \
      -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o /out/webhookd-demo \
      ./cmd/webhookd-demo

# --- mock-operator stage (separate target so the demo image stays lean) ---
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
```

A few production-shaped choices worth flagging:

- **`gcr.io/distroless/static-debian12:nonroot`** — minimal runtime,
  no shell, no package manager. Image clocks in around 6 MiB after
  the binary's added.
- **`-trimpath`** strips local filesystem paths from the binary —
  reproducible builds + smaller binary.
- **`-ldflags "-s -w"`** strips DWARF debug info. Drop these flags if
  you want stack traces with file/line numbers.
- **`CGO_ENABLED=0`** + static linking — no glibc dependency in the
  runtime image.
- **`USER nonroot`** — the binary runs as UID 65532. Pair with
  Kubernetes `securityContext.runAsNonRoot: true` (phase 12).

## docker-bake.hcl

The bake file groups multiple targets so a single command builds both
images. Match production webhookd's pattern: a default group for local
dev, a CI group for multi-arch.

### `docker-bake.hcl`

```hcl
# webhookd-demo image build targets.
# Local: `docker buildx bake`
# CI:    `docker buildx bake ci`

variable "REGISTRY" {
  default = "ghcr.io/example"
}

variable "VERSION" {
  default = "dev"
}

variable "COMMIT" {
  default = "unknown"
}

# Default group — runs on the host's native arch only.
group "default" {
  targets = ["webhookd-demo-local", "mock-operator-local"]
}

# CI group — multi-arch.
group "ci" {
  targets = ["webhookd-demo", "mock-operator"]
}

# --- shared base for the two binary targets ---
target "_base" {
  context    = "."
  dockerfile = "Dockerfile"
  args = {
    VERSION = "${VERSION}"
    COMMIT  = "${COMMIT}"
  }
  labels = {
    "org.opencontainers.image.source"      = "https://github.com/example/webhookd-demo"
    "org.opencontainers.image.revision"    = "${COMMIT}"
    "org.opencontainers.image.version"     = "${VERSION}"
    "org.opencontainers.image.title"       = "webhookd-demo"
    "org.opencontainers.image.description" = "Provider × Backend webhook receiver demo"
    "org.opencontainers.image.licenses"    = "MIT"
  }
}

# --- local builds ---
target "webhookd-demo-local" {
  inherits = ["_base"]
  target   = "webhookd-demo"
  tags     = ["webhookd-demo:dev"]
}

target "mock-operator-local" {
  inherits = ["_base"]
  target   = "mock-operator"
  tags     = ["mock-operator:dev"]
}

# --- CI multi-arch builds ---
target "webhookd-demo" {
  inherits  = ["_base"]
  target    = "webhookd-demo"
  platforms = ["linux/amd64", "linux/arm64"]
  tags = [
    "${REGISTRY}/webhookd-demo:${VERSION}",
    "${REGISTRY}/webhookd-demo:latest",
  ]
}

target "mock-operator" {
  inherits  = ["_base"]
  target    = "mock-operator"
  platforms = ["linux/amd64", "linux/arm64"]
  tags = [
    "${REGISTRY}/mock-operator:${VERSION}",
    "${REGISTRY}/mock-operator:latest",
  ]
}
```

## Build it

The justfile (phase 13 — actually committed in this directory) wraps
the bake invocation:

```bash
just bake
# docker buildx bake
# [+] Building 28.3s (15/15) FINISHED
# ...
# [internal] load metadata for gcr.io/distroless/static-debian12:nonroot
# ...
# webhookd-demo:dev built
# mock-operator:dev built
```

Verify:

```bash
docker images | grep -E '^(webhookd-demo|mock-operator) '
# webhookd-demo    dev    sha256:...   2 minutes ago    7.21MB
# mock-operator    dev    sha256:...   2 minutes ago    18.4MB
```

## CI multi-arch builds

The `ci` group emits multi-arch manifests via Docker's BuildKit
QEMU emulation. Locally:

```bash
docker buildx create --use --name multiarch || true
docker buildx bake ci \
  --set "*.tags=${REGISTRY}/webhookd-demo:test"
```

In a CI workflow, you'd add `--push` to push to the registry and
`--set "*.cache-from=type=gha"` / `--set "*.cache-to=type=gha,mode=max"`
to use the GitHub Actions cache exporter.

## Run the image locally

```bash
docker run --rm -p 8080:8080 -p 9090:9090 \
  -e WEBHOOK_DEMO_SECRET=topsecret \
  -e KUBECONFIG=/kube/config \
  -v "$HOME/.kube/config:/kube/config:ro" \
  -v "$(pwd)/webhookd.hcl:/webhookd.hcl:ro" \
  webhookd-demo:dev \
  --config /webhookd.hcl
```

The container reads the kubeconfig, opens listeners on `:8080` /
`:9090`, and behaves identically to the native binary.

## What we proved

- [x] Single Dockerfile, two runtime targets, multi-stage caching
- [x] Distroless + static binary = ~7 MiB image
- [x] Local + CI build flows share base config via `inherits`
- [x] Multi-arch via QEMU when needed

Next: [12-kustomize.md](12-kustomize.md) — deploying to Kubernetes.
