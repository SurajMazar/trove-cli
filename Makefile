# Trove build tooling.
#
#   make            show this help
#   make build      build ./bin/trove with version metadata
#   make test       run the test suite
#
# Override the Go command with GO=... (e.g. GO=go1.25.0). GOTOOLCHAIN is not
# forced here, so Go may download the toolchain required by go.mod.

GO        ?= go
BINARY    ?= trove
BIN_DIR   ?= bin
PKG       := github.com/SurajMazar/trove-cli
MAIN      := ./cmd/trove
COMPL_DIR ?= completions

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X $(PKG)/internal/version.Version=$(VERSION) \
	-X $(PKG)/internal/version.Commit=$(COMMIT) \
	-X $(PKG)/internal/version.Date=$(DATE)

GOFLAGS_BUILD := -trimpath -ldflags "$(LDFLAGS)"

.DEFAULT_GOAL := help

.PHONY: help build install test test-race cover lint vet fmt fmt-check tidy clean release snapshot completions

help: ## Show available targets
	@echo "Usage: make <target>"
	@echo
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build ./bin/trove with version, commit and date from git
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build $(GOFLAGS_BUILD) -o $(BIN_DIR)/$(BINARY) $(MAIN)
	@echo "built $(BIN_DIR)/$(BINARY) ($(VERSION))"

install: ## Install trove into GOBIN (or GOPATH/bin)
	CGO_ENABLED=0 $(GO) install $(GOFLAGS_BUILD) $(MAIN)

test: ## Run all tests
	$(GO) test ./...

test-race: ## Run all tests with the race detector
	$(GO) test -race ./...

cover: ## Run tests with coverage (coverage.out, coverage.html)
	$(GO) test -covermode=atomic -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -n 1
	$(GO) tool cover -html=coverage.out -o coverage.html
	@echo "open coverage.html for the annotated report"

lint: ## Run golangci-lint if installed (falls back to go vet)
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./...; \
	else \
		echo "golangci-lint is not installed; running 'go vet' instead."; \
		echo "Install it from https://golangci-lint.run/welcome/install/ (e.g. 'brew install golangci-lint')."; \
		$(GO) vet ./...; \
	fi

vet: ## Run go vet
	$(GO) vet ./...

fmt: ## Format all Go files in place
	gofmt -w .

fmt-check: ## Fail if any Go file is not gofmt-formatted
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "These files are not gofmt-formatted:"; echo "$$unformatted"; \
		echo "Run 'make fmt'."; exit 1; \
	fi

tidy: ## Tidy go.mod and go.sum
	$(GO) mod tidy

clean: ## Remove build, release and coverage artifacts
	rm -rf $(BIN_DIR) dist $(COMPL_DIR) coverage.out coverage.html

release: ## Publish a release with GoReleaser (needs a tag and GITHUB_TOKEN)
	goreleaser release --clean

snapshot: ## Build a local snapshot release into ./dist (nothing is published)
	goreleaser release --snapshot --clean

completions: build ## Generate bash/zsh/fish/powershell completions into ./completions
	TROVE_BIN="$(CURDIR)/$(BIN_DIR)/$(BINARY)" COMPLETIONS_DIR="$(COMPL_DIR)" ./scripts/completions.sh
