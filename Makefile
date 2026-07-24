# outpost — conventional Makefile entrypoint.
#
# outpost's build logic lives in scripts/*.sh (each "Replaces `make <x>`"); this
# Makefile is the thin, conventional front door that delegates to them, so
# `make build` / `make test` work like the sibling repos (cloudbox, bashy). The
# scripts remain the source of truth for ldflags/cross-compile/sibling-bootstrap.
#
# An agent-first equivalent lives alongside in DAG.md (`bashy dag build`, …).

.PHONY: help build build-all test test-headless tidy clean install

.DEFAULT_GOAL := help

help:  ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | \
	  awk 'BEGIN{FS=":.*## "}{printf "  \033[36m%-13s\033[0m %s\n", $$1, $$2}'

build:  ## Build outpost for the current platform into ./bin (bootstraps ../sh sibling)
	@./scripts/build.sh

build-all:  ## Cross-compile every release platform into ./bin
	@./scripts/build-all.sh

test:  ## Run Go tests in short mode (internal/agent/shell needs a real TTY)
	@go test -short ./...

test-headless:  ## Short tests minus internal/agent/shell (safe without a controlling TTY)
	@go test -short $$(go list ./... | grep -v internal/agent/shell)

tidy:  ## go mod tidy + go fmt + go vet
	@./scripts/tidy.sh

install:  ## Build + install into $$DHNT_BIN_DIR (default $$HOME/.local/bin)
	@./scripts/install-bin.sh

clean:  ## Remove build artifacts
	@./scripts/clean.sh
