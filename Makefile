# webhookd - Golang Webhook API

## Project Variables

PROJECT_NAME  := webhookd
PROJECT_OWNER := donaldgifford
DESCRIPTION   := Golang Webhook API
PROJECT_URL   := https://github.com/$(PROJECT_OWNER)/$(PROJECT_NAME)

## Go Variables

GO          ?= go
GO_PACKAGE  := github.com/$(PROJECT_OWNER)/$(PROJECT_NAME)
GOOS        ?= $(shell $(GO) env GOOS)
GOARCH      ?= $(shell $(GO) env GOARCH)

GOIMPORTS_LOCAL_ARG := -local github.com/donaldgifford

## Build Directories

BUILD_DIR      := build
BIN_DIR        := $(BUILD_DIR)/bin

## Version Information

COMMIT_HASH ?= $(shell git rev-parse --short HEAD 2>/dev/null)
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
CUR_VERSION ?= $(shell git describe --tags --exact-match 2>/dev/null || git describe --tags 2>/dev/null || echo "v0.0.0-$(COMMIT_HASH)")

## Build Variables

COVERAGE_OUT := coverage.out



###############
##@ Go Development

.PHONY: build
.PHONY: test test-all test-coverage
.PHONY: lint lint-fix fmt clean
.PHONY: run run-local test-api ci check
.PHONY: release-check release-local

## Build Targets

build: build-core ## Build everything (core)

build-core: ## Build core binary
	@ $(MAKE) --no-print-directory log-$@
	@mkdir -p $(BIN_DIR)
	@go build -ldflags "-X main.version=$(VERSION) -X main.commit=$(COMMIT_HASH)" -o $(BIN_DIR)/$(PROJECT_NAME) ./cmd/$(PROJECT_NAME)
	@echo "✓ Core binaries built"

## Testing

test: ## Run all tests with race detector
	@ $(MAKE) --no-print-directory log-$@
	@go test -v -race ./...

test-all: test ## Run all tests (core + plugins)

test-pkg: ## Test specific package (usage: make test-pkg PKG=./pkg/api)
	@ $(MAKE) --no-print-directory log-$@
	@go test -v -race $(PKG)

test-report: ## Run tests with coverage report then open
	@ $(MAKE) --no-print-directory log-$@
	@go test -coverprofile=$(COVERAGE_OUT) ./...
	@go tool cover -html=$(COVERAGE_OUT)

test-coverage: ## Run tests with coverage report
	@ $(MAKE) --no-print-directory log-$@
	@go test -v -race -coverprofile=$(COVERAGE_OUT) ./...


## Code Quality

lint: ## Run golangci-lint
	@ $(MAKE) --no-print-directory log-$@
	@golangci-lint run ./...

lint-fix: ## Run golangci-lint with auto-fix
	@ $(MAKE) --no-print-directory log-$@
	@golangci-lint run --fix ./...

fmt: ## Format code with gofmt and goimports
	@ $(MAKE) --no-print-directory log-$@
	@gofmt -s -w .
	@goimports -w $(GOIMPORTS_LOCAL_ARG) .

clean: ## Remove build artifacts
	@ $(MAKE) --no-print-directory log-$@
	@rm -rf $(BIN_DIR)/
	@rm -f $(COVERAGE_OUT)
	@go clean -cache
	@find . -name "*.test" -delete
	@echo "✓ Build artifacts cleaned"

## Application Services

run: ## Run CLI command
	@ $(MAKE) --no-print-directory log-$@
	@$(BIN_DIR)/$(PROJECT_NAME)

run-local: build ## Run exporter with local config
	@ $(MAKE) --no-print-directory log-$@
	@$(BIN_DIR)/$(PROJECT_NAME)

## Tools

ENVTEST_K8S_VERSION ?= 1.31.0
ENVTEST_BIN_DIR     := $(BUILD_DIR)/envtest
SETUP_ENVTEST       := $(ENVTEST_BIN_DIR)/setup-envtest

$(SETUP_ENVTEST):
	@mkdir -p $(ENVTEST_BIN_DIR)
	@GOBIN=$(abspath $(ENVTEST_BIN_DIR)) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest

tools-envtest: $(SETUP_ENVTEST) ## Install envtest binaries (kube-apiserver, etcd) for integration tests
	@ $(MAKE) --no-print-directory log-$@
	@$(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir=$(ENVTEST_BIN_DIR) -p path
	@echo "✓ envtest binaries materialized in $(ENVTEST_BIN_DIR)"

# KUBEBUILDER_ASSETS exports the path to the envtest binaries when set,
# letting `go test` find them. Wire it into `make test` only when the
# binaries already exist — avoids forcing every test invocation to
# install setup-envtest.
ifneq ($(wildcard $(SETUP_ENVTEST)),)
export KUBEBUILDER_ASSETS := $(shell $(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir=$(abspath $(ENVTEST_BIN_DIR)) -p path 2>/dev/null)
endif

## Helm Chart Toolchain

CHART_DIR              := charts/webhookd
CHART_NAME             := webhookd
HELM_UNITTEST_VERSION  ?= 1.0.3
HELM_REGISTRY          ?= ghcr.io/donaldgifford/charts

.PHONY: chart-tools
.PHONY: helm-lint helm-template helm-template-ci helm-package
.PHONY: helm-unittest helm-test helm-ct-lint helm-ct-list-changed helm-ct-install
.PHONY: helm-docs helm-docs-check helm-diff-check helm-cr-package helm-push

chart-tools: ## Install helm plugins not managed by mise (helm-unittest)
	@ $(MAKE) --no-print-directory log-$@
	@helm plugin list 2>/dev/null | awk '{print $$1}' | grep -qx unittest \
		|| helm plugin install https://github.com/helm-unittest/helm-unittest --version $(HELM_UNITTEST_VERSION)
	@echo "✓ helm-unittest plugin installed (version $(HELM_UNITTEST_VERSION))"

helm-lint: ## Lint the webhookd Helm chart with `helm lint` against ci-values.yaml
	@ $(MAKE) --no-print-directory log-$@
	@helm lint $(CHART_DIR) -f $(CHART_DIR)/ci/ci-values.yaml

helm-template: ## Render the chart with default values
	@ $(MAKE) --no-print-directory log-$@
	@helm template $(CHART_NAME) $(CHART_DIR)

helm-template-ci: ## Render the chart with CI values overrides
	@ $(MAKE) --no-print-directory log-$@
	@helm template $(CHART_NAME) $(CHART_DIR) -f $(CHART_DIR)/ci/ci-values.yaml

helm-package: ## Package the chart into a versioned `.tgz`
	@ $(MAKE) --no-print-directory log-$@
	@helm package $(CHART_DIR)

helm-unittest: chart-tools ## Run helm-unittest cases under the chart
	@ $(MAKE) --no-print-directory log-$@
	@helm unittest $(CHART_DIR)

helm-test: helm-lint helm-unittest ## Run helm lint and helm-unittest
	@echo "✓ Helm chart lint + unit tests passed"

helm-ct-lint: ## Run `ct lint` against the chart directory
	@ $(MAKE) --no-print-directory log-$@
	@ct lint --config ct.yaml --debug

helm-ct-list-changed: ## List charts changed since the target branch
	@ $(MAKE) --no-print-directory log-$@
	@ct list-changed --config ct.yaml

helm-ct-install: ## Run `ct install` against the current kube context (kind)
	@ $(MAKE) --no-print-directory log-$@
	@ct install --config ct.yaml --debug

helm-docs: ## Regenerate chart README via helm-docs
	@ $(MAKE) --no-print-directory log-$@
	@helm-docs --chart-search-root charts

helm-docs-check: ## Fail when `helm-docs` would change committed README files
	@ $(MAKE) --no-print-directory log-$@
	@helm-docs --chart-search-root charts
	@git diff --exit-code -- charts/**/README.md \
		|| { echo "helm-docs drift detected — run 'make helm-docs' and commit the result"; exit 1; }

helm-diff-check: ## Diff the locally-rendered chart against an installed release (RELEASE=name required)
	@ $(MAKE) --no-print-directory log-$@
	@if [ -z "$(RELEASE)" ]; then echo "Error: RELEASE is required. Usage: make helm-diff-check RELEASE=webhookd"; exit 1; fi
	@helm diff upgrade $(RELEASE) $(CHART_DIR)

helm-cr-package: ## Package the chart with chart-releaser into `.cr-release-packages/`
	@ $(MAKE) --no-print-directory log-$@
	@cr package $(CHART_DIR)

helm-push: helm-package ## Package and push the chart to the OCI registry (HELM_REGISTRY)
	@ $(MAKE) --no-print-directory log-$@
	@helm push $(CHART_NAME)-*.tgz oci://$(HELM_REGISTRY)

## License Compliance

license-check: ## Check dependency licenses against allowed list
	@ $(MAKE) --no-print-directory log-$@
	@go-licenses check ./... --allowed_licenses=Apache-2.0,MIT,BSD-2-Clause,BSD-3-Clause,ISC,MPL-2.0

license-report: ## Generate CSV report of all dependency licenses
	@ $(MAKE) --no-print-directory log-$@
	@go-licenses report ./... --template=.github/licenses-csv.tpl

## CI/CD

ci: lint test build license-check ## Run CI pipeline (lint + test + build + license check)
	@ $(MAKE) --no-print-directory log-$@
	@echo "✓ CI pipeline complete"

check: lint test ## Quick pre-commit check (lint + test)
	@ $(MAKE) --no-print-directory log-$@
	@echo "✓ Pre-commit checks passed"

# =============================================================================
# Release Targets
# =============================================================================

release: ## Create release (use with TAG=v1.0.0)
	@ $(MAKE) --no-print-directory log-$@
	@if [ -z "$(TAG)" ]; then \
		echo "Error: TAG is required. Usage: make release TAG=v1.0.0"; \
		exit 1; \
	fi
	git tag -a $(TAG) -m "Release $(TAG)"
	git push origin $(TAG)

release-check:
	@ $(MAKE) --no-print-directory log-$@
	goreleaser check


release-local: ## Test goreleaser without publishing
	@ $(MAKE) --no-print-directory log-$@
	goreleaser release --snapshot --clean --skip=publish --skip=sign


########################################################################
## Self-Documenting Makefile Help                                     ##
## https://marmelab.com/blog/2016/02/29/auto-documented-makefile.html ##
########################################################################

########
##@ Help

.PHONY: help
help:   ## Display this help
	@awk -v "col=\033[36m" -v "nocol=\033[0m" ' \
		BEGIN { FS = ":.*##" ; printf "Usage:\n  make %s<target>%s\n\n", col, nocol } \
		/^[a-zA-Z_0-9-]+:.*?##/ { printf "  %s%-25s%s %s\n", col, $$1, nocol, $$2 } \
		/^##@/ { printf "\n%s%s%s\n", nocol, substr($$0, 5), nocol } \
	' $(MAKEFILE_LIST)

## Log Pattern
## Automatically logs what a target does by extracting its ## comment
log-%:
	@grep -h -E '^$*:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN { FS = ":.*?## " }; { printf "\033[36m==> %s\033[0m\n", $$2 }'
