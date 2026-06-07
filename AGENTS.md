# Repository Guidelines

## Project Structure & Module Organization

This is a Go 1.25 module for the `outpost` host agent. CLI entry points live in `cmd/outpost/`; `cmd/outpost-vk/` is a standalone virtual-kubelet PoC runner. The Phase-3 CNI plugin source lives at `internal/agent/runtime/image/cni/` and is compiled inside the runtime container by the multi-stage Dockerfile that `outpost cluster build-runtime` drives. Core agent code is under `internal/agent/`, with focused subpackages including `admincore`, `adminui`, `mcpapi`, `shell`, `upgrade`, `vkpodman`, `runtime`, `userkube`, `peerhosts`, `ollama`, `sysinfo`, `osversion`, and `ycode`. Operator docs are in `docs/`; embedded copies for `outpost docs` live in `cmd/outpost/embedded_docs/` and must stay synced. The shell runner depends on a fork of `mvdan.cc/sh/v3` resolved via the sibling-path directive `replace mvdan.cc/sh/v3 => ../sh` in `go.mod`. Inside the dhnt umbrella `../sh` points at the `dhnt/sh` submodule; standalone clones run `./scripts/bootstrap-siblings.sh` to clone it into `../sh` at the SHA pinned in `.sibling-pins`.

## Build, Test, and Development Commands

There is no Makefile — the canonical entry points are bash scripts under `scripts/`. They work both under regular bash and under `outpost shell`, so a user with only `outpost` + a Go toolchain installed can rebuild from source (no system git, no make, no coreutils setup beyond what `outpost shell` already provides).

- `./scripts/bootstrap-siblings.sh` materializes sibling-path replace targets (`../sh`) from `.sibling-pins`; run once on a fresh standalone clone before `./scripts/build.sh`. No-op inside the dhnt umbrella. Prefers `outpost git` when on PATH; falls back to system `git`.
- `./scripts/build.sh` builds `./cmd/outpost` into `./bin/outpost`; set `RELEASE_TAG=vX.Y.Z` to stamp release metadata. Honors `$GOOS`/`$GOARCH`/`$CGO_ENABLED` from env.
- `./scripts/build-all.sh` cross-compiles all release platforms (darwin/linux/windows × amd64/arm64).
- `./scripts/install-bin.sh` installs the built binary to `$INSTALL_DIR`, defaulting to `~/bin`.
- `./scripts/tidy.sh` runs `go mod tidy`, `go fmt ./...`, and `go vet ./...`.
- `./scripts/clean.sh` removes `./bin` and stray test/coverage artifacts.
- `go test ./...` runs the full test suite. Use package filters while iterating, for example `go test ./internal/agent/adminui -run TestE2E`.
- `go run ./cmd/outpost start` runs the daemon from source; `go run ./cmd/outpost docs settings` checks embedded docs output.

Self-rebuild from an existing outpost install (no system git, no make):

```
outpost git clone https://github.com/qiangli/outpost.git
cd outpost
outpost shell ./scripts/bootstrap-siblings.sh
outpost shell ./scripts/build.sh
```

## Coding Style & Naming Conventions

Use standard Go formatting (`gofmt`) and keep package boundaries narrow. Prefer small, testable functions in the relevant `internal/agent/*` package over broad helpers. CLI commands use Unix-style subcommands (`apps add`, `outbound rm`); MCP tools use verb-noun names such as `outpost_upsert_app`. Keep persisted config keys and user-facing settings aligned with `docs/settings.md`.

## Testing Guidelines

Place tests beside the code as `*_test.go`. Favor table-driven tests for validation, config, and route behavior. Coverage already exists for admin UI, MCP protocol round trips, shell behavior, upgrade flow, and CLI docs drift; update it when touching those areas. Run `go test ./...` before submitting.

## Commit & Pull Request Guidelines

Recent commits use short, imperative subjects with an optional scope prefix, for example `upgrade: CLI path also retains outpost.previous` or `fix: Probe normalizes both sides`. Keep commits focused and mention user-visible behavior when relevant. PRs should include a concise description, test results, linked issues when applicable, and screenshots for admin UI changes.

## Security & Configuration Tips

The admin listener is intended to bind loopback by default. Treat bearer tokens, pairing data, SSH settings, and config files as sensitive; avoid logging secrets or widening bind addresses without explicit intent. When changing canonical docs, sync `docs/<topic>.md` to `cmd/outpost/embedded_docs/<topic>.md` so `cmd/outpost/docs_test.go` stays green.
