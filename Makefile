# Makefile for cloudflare-tunnel-gateway-controller
# Run `make help` to list all available targets.

# Build variables
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
GITSHA      := $(shell git rev-parse HEAD 2>/dev/null || echo "unknown")
LDFLAGS     := -X main.Version=$(VERSION) -X main.Gitsha=$(GITSHA)
BIN_DIR     := bin
CHART_PATH  := charts/cloudflare-tunnel-gateway-controller
API_REF     := docs/reference/api.md
GENERATED   := api/v1alpha1/zz_generated.deepcopy.go $(CHART_PATH)/crds $(API_REF)
HELM_DOCS_VERSION = $(shell go list -C hack/tools -m -f '{{.Version}}' github.com/norwoodj/helm-docs)

.PHONY: all build build-proxy test test-race test-coverage lint lint-fix lint-md helm-lint helm-test helm-docs \
        helm-template docs-serve docs-build container ci-go ci-helm ci-docs check-deps help \
        generate verify-generated

##@ Build

build: ## Build the controller binary
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/controller ./cmd/controller

build-proxy: ## Build the proxy binary
	go build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/proxy ./cmd/proxy

##@ Code generation

# The generators are pinned in hack/tools/go.mod, a separate module so their
# dependencies stay out of the controller's go.mod and vendor/.
generate: ## Regenerate deepcopy code, CRDs and the API reference from api/v1alpha1
	go build -C hack/tools -o $(CURDIR)/$(BIN_DIR)/controller-gen sigs.k8s.io/controller-tools/cmd/controller-gen
	go build -C hack/tools -o $(CURDIR)/$(BIN_DIR)/crd-ref-docs github.com/elastic/crd-ref-docs
	$(BIN_DIR)/controller-gen object paths=./api/... crd paths=./api/... output:crd:dir=$(CHART_PATH)/crds
	$(BIN_DIR)/crd-ref-docs --log-level ERROR --source-path ./api --renderer markdown \
		--config hack/crd-ref-docs/config.yaml --templates-dir hack/crd-ref-docs/templates \
		--output-path $(API_REF)

verify-generated: generate ## Fail if the generated files differ from what is committed
	@changes="$$(git status --porcelain -- $(GENERATED))"; \
	if [ -n "$$changes" ]; then \
		echo "Generated files are out of date; run 'make generate' and commit the result:"; \
		echo "$$changes"; \
		git --no-pager diff -- $(GENERATED); \
		exit 1; \
	fi

##@ Testing

test: ## Run all tests
	go test ./...

test-race: ## Run all tests with race detector
	go test -race ./...

test-coverage: ## Run all tests with coverage report (coverage.out)
	go test -coverprofile=coverage.out ./...

##@ Linting

lint: ## Run golangci-lint
	golangci-lint run --timeout=5m --build-tags e2e,conformance,envtest

lint-fix: ## Run golangci-lint with auto-fix
	golangci-lint run --timeout=5m --build-tags e2e,conformance,envtest --fix

lint-md: ## Lint all Markdown files
	markdownlint-cli2 '**/*.md'

##@ Helm

helm-lint: ## Lint the Helm chart
	helm lint $(CHART_PATH)

helm-test: ## Run Helm unit tests
	helm unittest $(CHART_PATH)

# helm-docs writes its version into the README footer, and only a build that
# sets main.version gets one, so the pin from hack/tools/go.mod goes in here.
helm-docs: ## Regenerate chart README from values.yaml with the pinned helm-docs
	go build -C hack/tools -ldflags "-X main.version=$(HELM_DOCS_VERSION:v%=%)" \
		-o $(CURDIR)/$(BIN_DIR)/helm-docs github.com/norwoodj/helm-docs/cmd/helm-docs
	$(BIN_DIR)/helm-docs --chart-search-root $(CHART_PATH)

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
	go test -race ./... && golangci-lint run --timeout=5m --build-tags e2e,conformance,envtest

ci-helm: helm-docs ## Run all Helm CI gates (test + lint + docs)
	git diff --exit-code $(CHART_PATH)/README.md && \
	helm unittest $(CHART_PATH) && \
	helm lint $(CHART_PATH) && \
	./hack/chart-reuse-values.sh $(CHART_PATH)

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
	@helm plugin list 2>/dev/null | grep -q unittest && echo "OK: helm-unittest" || echo "MISSING: helm-unittest  helm plugin install https://github.com/helm-unittest/helm-unittest.git --verify=false"

help: ## Print this help message
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} \
		/^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 } \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)
