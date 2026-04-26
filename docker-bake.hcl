# docker-bake.hcl — declarative buildx targets for webhookd.
#
# Local dev:
#     docker buildx bake             # builds webhookd:dev for local platform
#     docker buildx bake webhookd    # same
#
# CI (PR + post-merge multi-arch):
#     docker buildx bake ci          # multi-arch, tagged for ghcr.io push
#
# See IMPL-0001 Phase 0. The ci.yml docker-build job invokes the `ci` target
# and overrides cache + output via `set:` to push to ghcr.io.

variable "REGISTRY" {
  default = "ghcr.io"
}

variable "REPO" {
  default = "donaldgifford/webhookd"
}

variable "VERSION" {
  default = "dev"
}

variable "COMMIT" {
  default = "unknown"
}

# -----------------------------------------------------------------------------
# Groups
# -----------------------------------------------------------------------------

# Default: build a local single-platform image for development.
group "default" {
  targets = ["webhookd-local"]
}

# CI: multi-arch image suitable for registry push.
group "ci" {
  targets = ["webhookd"]
}

# -----------------------------------------------------------------------------
# Targets
# -----------------------------------------------------------------------------

# _common holds the shared dockerfile, build args, and OCI labels.
# All concrete targets inherit from it.
target "_common" {
  context    = "."
  dockerfile = "Dockerfile"

  args = {
    VERSION = "${VERSION}"
    COMMIT  = "${COMMIT}"
  }

  labels = {
    "org.opencontainers.image.source"   = "https://github.com/donaldgifford/webhookd"
    "org.opencontainers.image.title"    = "webhookd"
    "org.opencontainers.image.version"  = "${VERSION}"
    "org.opencontainers.image.revision" = "${COMMIT}"
    "org.opencontainers.image.licenses" = "Apache-2.0"
  }
}

# webhookd-local: convenience target for `docker buildx bake` without args.
# Single platform (your machine's), tagged for local docker daemon use.
target "webhookd-local" {
  inherits = ["_common"]
  tags     = ["webhookd:dev"]
}

# webhookd: multi-arch CI target.
target "webhookd" {
  inherits = ["_common"]

  tags = [
    "${REGISTRY}/${REPO}:${VERSION}",
    "${REGISTRY}/${REPO}:${COMMIT}",
  ]

  platforms = [
    "linux/amd64",
    "linux/arm64",
  ]
}
