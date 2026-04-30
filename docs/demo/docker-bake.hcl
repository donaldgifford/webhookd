# webhookd-demo image build targets.
#   `docker buildx bake`         — local single-arch build (default group)
#   `docker buildx bake ci`      — multi-arch build for CI/registry push

variable "REGISTRY" {
  default = "ghcr.io/example"
}

variable "VERSION" {
  default = "dev"
}

variable "COMMIT" {
  default = "unknown"
}

# Default group: native arch only — fast local rebuilds.
group "default" {
  targets = ["webhookd-demo-local", "mock-operator-local"]
}

# CI group: multi-arch images for the registry.
group "ci" {
  targets = ["webhookd-demo", "mock-operator"]
}

# Shared base — every target inherits these.
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
