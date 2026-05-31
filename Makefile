BIN         ?= outpost
PKG         := ./cmd/outpost
OUT_DIR     := bin
INSTALL_DIR ?= $(HOME)/bin

# Version metadata stamped via -ldflags.
#
# Without these, outpost's runtime/debug.ReadBuildInfo walks UP the
# directory tree to find the nearest .git — which in the dhnt umbrella
# layout lands on the umbrella's HEAD, not this submodule's. The
# COMMIT / DIRTY injection makes the version label match the outpost
# commit unambiguously regardless of how outpost is checked out.
#
# RELEASE_TAG, when non-empty, additionally stamps a semver tag.
# Set by the GitHub Actions release workflow from the triggering git
# tag (e.g. RELEASE_TAG=v0.2.0). Local `make build` leaves it empty
# and the binary surfaces only the commit short-sha.
COMMIT      := $(shell git rev-parse --short HEAD 2>/dev/null)
DIRTY       := $(shell git diff --quiet 2>/dev/null && echo false || echo true)
RELEASE_TAG ?=

LDFLAGS := -X github.com/qiangli/outpost/internal/agent.ldCommit=$(COMMIT) \
           -X github.com/qiangli/outpost/internal/agent.ldDirty=$(DIRTY)
ifneq ($(RELEASE_TAG),)
LDFLAGS += -X github.com/qiangli/outpost/internal/agent.releaseTag=$(RELEASE_TAG)
endif

# Cross-build matrix. Mirrors .github/workflows/release.yml so a local
# `make build-all` produces the same set of artifacts the release flow
# uploads to GH. CGO_ENABLED=0 is critical for cross-compile — outpost
# has no cgo dependencies, so this is correct everywhere.
PLATFORMS := darwin-amd64 darwin-arm64 linux-amd64 linux-arm64 windows-amd64 windows-arm64

.PHONY: help build build-all install clean tidy bootstrap

help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "Targets:\n"} /^[a-zA-Z_-]+:.*?##/ { printf "  %-12s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

build: ## Build the outpost binary for the current platform into ./bin
	@mkdir -p $(OUT_DIR)
	go build -ldflags "$(LDFLAGS)" -trimpath -o $(OUT_DIR)/$(BIN) $(PKG)

build-all: ## Cross-compile outpost for all release platforms (darwin/linux/windows × amd64/arm64)
	@mkdir -p $(OUT_DIR)
	@for p in $(PLATFORMS); do \
	  os=$${p%-*}; arch=$${p##*-}; \
	  out="$(OUT_DIR)/$(BIN)-$$p"; \
	  if [ "$$os" = "windows" ]; then out="$$out.exe"; fi; \
	  echo "  → $$out"; \
	  GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -trimpath -o "$$out" $(PKG) || exit 1; \
	done
	@ls -lh $(OUT_DIR)/$(BIN)-*

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
