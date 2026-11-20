MODULE      := github.com/m4drak/tikker
BIN         := bin/tikker
GO          ?= go
GOLANGCI    ?= golangci-lint

VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE        ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
VERSION_PKG := $(MODULE)/internal/version
LDFLAGS     := -s -w \
	-X $(VERSION_PKG).Version=$(VERSION) \
	-X $(VERSION_PKG).Commit=$(COMMIT) \
	-X $(VERSION_PKG).Date=$(DATE)

ARGS        ?= version
REDOCLY     ?= npx --yes @redocly/cli@1.34.5

# Container image (deploy/docker/Dockerfile). Build args reuse the version values above.
IMAGE            ?= tikker
DOCKERFILE       ?= deploy/docker/Dockerfile
DOCKER_PLATFORMS ?= linux/amd64,linux/arm64,linux/arm/v7
DOCKER_BUILD_ARGS := --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE)

# Tool versions are pinned so that local runs and CI use the same binaries.
# go-licenses is run through `go run`, so nothing has to be installed globally.
GO_LICENSES   ?= github.com/google/go-licenses/v2@v2.0.1
GOVULNCHECK   ?= golang.org/x/vuln/cmd/govulncheck@v1.9.0
# Allowlist from SPEC §11 (GPLv3-compatible licenses).
LICENSES_ALLOWED := MIT,BSD-2-Clause,BSD-3-Clause,ISC,0BSD,Apache-2.0,MPL-2.0,LGPL-2.1,LGPL-3.0,GPL-3.0

.DEFAULT_GOAL := build
.PHONY: build test lint run clean licenses vuln ci openapi-lint docker docker-buildx

build: ## Build a static binary (CGO disabled) into bin/
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/tikker

test: ## Run tests with the race detector and coverage
	$(GO) test -race -cover ./...

lint: ## Run golangci-lint (includes go vet and gofmt/goimports checks)
	$(GOLANGCI) run ./...

run: build ## Build and run the binary; pass arguments with ARGS="..."
	./$(BIN) $(ARGS)

clean: ## Remove build artifacts
	rm -rf bin dist coverage.out

licenses: ## Check dependency licenses against the SPEC §11 allowlist (go-licenses)
	$(GO) run $(GO_LICENSES) check ./... --allowed_licenses=$(LICENSES_ALLOWED)

vuln: ## Scan for known vulnerabilities (govulncheck)
	$(GO) run $(GOVULNCHECK) ./...

ci: lint test build licenses vuln openapi-lint ## Run the CI checks locally (run `reuse lint` separately)

openapi-lint: ## Lint the OpenAPI contract (api/openapi.yaml)
	$(REDOCLY) lint

docker: ## Build the container image for the local platform and load it into the Docker daemon
	docker build -f $(DOCKERFILE) $(DOCKER_BUILD_ARGS) -t $(IMAGE):$(VERSION) .

docker-buildx: ## Build the multi-arch image (amd64, arm64, arm/v7) into dist/ as an OCI archive
	mkdir -p dist
	docker buildx build --platform $(DOCKER_PLATFORMS) -f $(DOCKERFILE) $(DOCKER_BUILD_ARGS) \
		-t $(IMAGE):$(VERSION) --output type=oci,dest=dist/$(IMAGE)-$(VERSION).oci.tar .
