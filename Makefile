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

.DEFAULT_GOAL := build
.PHONY: build test lint run clean

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
