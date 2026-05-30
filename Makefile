BIN         ?= outpost
PKG         := ./cmd/outpost
OUT_DIR     := bin
INSTALL_DIR ?= $(HOME)/bin

# RELEASE_TAG, when non-empty, is baked into BuildInfo via ldflags so
# cloudbox can compare semver tags instead of opaque commit shas. Set
# by the GitHub Actions release workflow from the triggering git tag
# (e.g. RELEASE_TAG=v0.2.0). Local `make build` leaves it empty —
# BuildInfo falls back to the commit sha for ad-hoc builds.
RELEASE_TAG ?=
LDFLAGS     := $(if $(RELEASE_TAG),-X github.com/qiangli/outpost/internal/agent.releaseTag=$(RELEASE_TAG),)

.PHONY: help build install clean tidy update-sh

help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "Targets:\n"} /^[a-zA-Z_-]+:.*?##/ { printf "  %-10s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

build: ## Build the outpost binary into ./bin (set RELEASE_TAG=vX.Y.Z to stamp a tag)
	@mkdir -p $(OUT_DIR)
	go build $(if $(LDFLAGS),-ldflags "$(LDFLAGS)") -o $(OUT_DIR)/$(BIN) $(PKG)

install: build ## Install outpost into $(INSTALL_DIR) (default: ~/bin)
	@mkdir -p $(INSTALL_DIR)
	install -m 0755 $(OUT_DIR)/$(BIN) $(INSTALL_DIR)/$(BIN)
	@echo "installed $(INSTALL_DIR)/$(BIN)"

clean: ## Remove build artifacts
	rm -rf $(OUT_DIR)
	rm -f $(BIN) *.test *.out

tidy: ## go mod tidy + go fmt + go vet
	go mod tidy
	go fmt ./...
	go vet ./...

bootstrap: ## Materialize sibling-path replace targets (../sh) from .sibling-pins
	./scripts/bootstrap-siblings.sh
