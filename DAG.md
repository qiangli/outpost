---
name: outpost
description: Build/test/lint targets for outpost as a bashy dag pipeline (dogfood of the new Makefile)
---

# outpost — DAG task file

The agent-first equivalent of this repo's `Makefile` (itself a thin wrapper over
`scripts/*.sh`), runnable with `bashy dag`:

```bash
bashy dag --list            # available targets
bashy dag build             # build outpost into ./bin
bashy dag test-headless     # short tests, TTY-free
bashy dag --json test       # machine-readable envelope for an agent
```

The bodies delegate to the existing `scripts/*.sh` (the source of truth for
ldflags, cross-compile matrix, and the `../sh` sibling bootstrap). `dag` adds
the explicit dependency graph and structured/JSON output for agents.

## Tasks

### build
Build outpost for the current platform into ./bin. Bootstraps the ../sh sibling
(go.mod: replace mvdan.cc/sh/v3 => ../sh) first, so a fresh clone builds in one
command.
Sources: cmd/, internal/, go.mod, go.sum
Generates: bin/outpost
Effects: write, net

```bash
./scripts/build.sh
```

### build-all
Cross-compile outpost for every release platform into ./bin.
Generates: bin
Effects: write, net

```bash
./scripts/build-all.sh
```

### test
Run Go tests in short mode. NOTE: internal/agent/shell drives ergochat/readline
against a PTY and hangs without a controlling TTY — use `test-headless` in a
headless run.
Effects: read, net

```bash
go test -short ./...
```

### test-headless
Short tests minus internal/agent/shell — safe in a TTY-less environment.
Effects: read, net

```bash
go test -short $(go list ./... | grep -v internal/agent/shell)
```

### tidy
go mod tidy + go fmt + go vet.
Effects: write, net

```bash
./scripts/tidy.sh
```

### install
Build then install the binary into $HOME/bin.
Requires: build
Effects: write

```bash
./scripts/install-bin.sh
```

### clean
Remove build artifacts.
Effects: destroy

```bash
./scripts/clean.sh
```
