# Makefile for cloudflare-tunnel-gateway-controller
# Run `make help` to list all available targets.

# Build variables
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
GITSHA      := $(shell git rev-parse HEAD 2>/dev/null || echo "unknown")
LDFLAGS     := -X main.Version=$(VERSION) -X main.Gitsha=$(GITSHA)
BIN_DIR     := bin
CHART_PATH  := charts/cloudflare-tunnel-gateway-controller

.PHONY: all build build-proxy test test-race test-coverage lint lint-fix lint-md helm-lint helm-test helm-docs \
        helm-template docs-serve docs-build container ci-go ci-helm ci-docs check-deps help

##@ Build

build: ## Build the controller binary
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/controller ./cmd/controller

build-proxy: ## Build the proxy binary
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/proxy ./cmd/proxy

##@ Testing

test: ## Run all tests
	go test ./...

test-race: ## Run all tests with race detector
	go test -race ./...

test-coverage: ## Run all tests with coverage report (coverage.out)
	go test -coverprofile=coverage.out ./...

##@ Linting

lint: ## Run golangci-lint
	golangci-lint run --timeout=5m

lint-fix: ## Run golangci-lint with auto-fix
	golangci-lint run --timeout=5m --fix

lint-md: ## Lint all Markdown files
	markdownlint-cli2 '**/*.md'

##@ Helm

helm-lint: ## Lint the Helm chart
	helm lint $(CHART_PATH)

helm-test: ## Run Helm unit tests
	helm unittest $(CHART_PATH)

helm-docs: ## Regenerate chart README from values.yaml
	helm-docs $(CHART_PATH)

helm-template: ## Template the chart locally for debugging
	helm template test $(CHART_PATH) \
		--values $(CHART_PATH)/examples/basic-values.yaml

##@ Documentation

docs-serve: ## Start local MkDocs preview server
	mkdocs serve

docs-build: ## Build the MkDocs site (strict mode)
	mkdocs build --strict

##@ Container

container: ## Build both container images (controller and proxy)
	podman build --tag cloudflare-tunnel-gateway-controller:dev --file Containerfile .
	podman build --tag cloudflare-tunnel-gateway-controller-proxy:dev --file Containerfile.proxy .

##@ CI

ci-go: ## Run all Go CI gates (test + lint)
	go test -race ./... && golangci-lint run --timeout=5m

ci-helm: ## Run all Helm CI gates (test + lint + docs)
	helm unittest $(CHART_PATH) && \
	helm lint $(CHART_PATH) && \
	helm-docs $(CHART_PATH) && git diff --exit-code $(CHART_PATH)/README.md

ci-docs: ## Run docs CI gate
	mkdocs build --strict

##@ Misc

check-deps: ## Check all required tools are installed
	@which go > /dev/null 2>&1 && echo "OK: go" || echo "MISSING: go           https://go.dev/dl/"
	@which golangci-lint > /dev/null 2>&1 && echo "OK: golangci-lint" || echo "MISSING: golangci-lint  https://golangci-lint.run/usage/install/"
	@which helm > /dev/null 2>&1 && echo "OK: helm" || echo "MISSING: helm          https://helm.sh/docs/intro/install/"
	@which mkdocs > /dev/null 2>&1 && echo "OK: mkdocs" || echo "MISSING: mkdocs        pip install -r requirements-docs.txt"
	@which podman > /dev/null 2>&1 && echo "OK: podman" || echo "MISSING: podman        https://podman.io/getting-started/installation"
	@which markdownlint-cli2 > /dev/null 2>&1 && echo "OK: markdownlint-cli2" || echo "MISSING: markdownlint-cli2  npm install -g markdownlint-cli2"
	@helm plugin list 2>/dev/null | grep -q unittest && echo "OK: helm-unittest" || echo "MISSING: helm-unittest  helm plugin install https://github.com/helm-unittest/helm-unittest"
	@which helm-docs > /dev/null 2>&1 && echo "OK: helm-docs" || echo "MISSING: helm-docs     https://github.com/norwoodj/helm-docs#installation"

help: ## Print this help message
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} \
		/^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 } \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)
