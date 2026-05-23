BIN     ?= outpost
PKG     := ./cmd/outpost
OUT_DIR := bin

.PHONY: help build install clean tidy update-sh

help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "Targets:\n"} /^[a-zA-Z_-]+:.*?##/ { printf "  %-10s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

build: ## Build the outpost binary into ./bin
	@mkdir -p $(OUT_DIR)
	go build -o $(OUT_DIR)/$(BIN) $(PKG)

install: ## Install outpost into $GOBIN / $GOPATH/bin
	go install $(PKG)

clean: ## Remove build artifacts
	rm -rf $(OUT_DIR)
	rm -f $(BIN) *.test *.out

tidy: ## go mod tidy + go fmt + go vet
	go mod tidy
	go fmt ./...
	go vet ./...

update-sh: ## Bump external/sh to upstream master HEAD
	git submodule update --remote external/sh
	@echo "Now: git add external/sh && git commit"
