---
name: outpost-deploy
description: cross-platform build / test / release for outpost (+ paired bashy). QA runs per-OS; release is a gated tag.
---
# deploy
## Tasks

### bootstrap
Materialize sibling deps (coreutils/sh) so the build resolves ../coreutils, ../sh.
Effects: write
```bash
./scripts/bootstrap-siblings.sh
```

### build-all
Cross-compile outpost for every release platform on ONE host (CGO_ENABLED=0).
Requires: bootstrap
Effects: write
```bash
./scripts/build-all.sh
```

### test-host
The per-platform test GATE — run on a runner OF THAT OS (a darwin binary can't
run on linux, so each qa-<os> is this target on its own runner). Uses `bashy go`
so the ONLY per-host prerequisite is bashy — bashy self-provisions the pinned Go
toolchain (+ git, coreutils). Shell tests need a controlling TTY, so they're
excluded in headless CI (run them on a real dev box).
Effects: write
```bash
./scripts/bootstrap-siblings.sh
bashy go test -short $(bashy go list ./... | grep -v 'internal/agent/shell')
```

### qa-runtime
RUNTIME smoke on THIS OS — the built binary actually runs and its core surfaces
work. Catches per-OS RUNTIME bugs the compile+unit gate misses (Windows
shell-exec / in-process pipe / PTY / real-git). GENERALIZED via bashy scripting:
this ONE script runs identically on every OS runner (bashy is the same pure-Go
binary everywhere) — `runs-on` just picks the box. No bespoke per-OS test code.
Requires: build-all
Effects: write
```bash
set -e
BIN=./bin/outpost; [ -x "$BIN" ] || BIN=./bin/outpost.exe
fail(){ echo "FAIL qa-runtime: $1"; exit 1; }
# 1. the binary runs on this OS (build provenance)
"$BIN" version | head -1
# 2. in-process shell engine (the Windows exec/PTY hot spot)
[ "$("$BIN" shell -c 'echo runtime-ok')" = "runtime-ok" ] || fail "shell -c echo"
# 3. pipe + in-process coreutils (the Windows in-process-PIPE bug)
[ "$("$BIN" shell -c 'printf "a\nb\n" | grep b')" = "b" ] || fail "shell pipe+grep"
# 4. real git surface (git-scm on Windows / system git on unix)
"$BIN" git --version >/dev/null 2>&1 || fail "git --version"
echo ">> qa-runtime PASS on $("$BIN" version | head -1)"
```
(Future: an isolated daemon-integration smoke — start with a temp OUTPOST_ADMIN_ADDR
+ config dir so it doesn't fight the host's outpost, probe /healthz, register-loopback.)

### qa-mac
macOS QA gate = build+unit (test-host) + runtime smoke (qa-runtime) on a macOS
runner (dragon). Run by the workflow on `runs-on: [self-hosted, sdlc, macos]`.
Requires: test-host qa-runtime
Effects: write
```bash
echo ">> qa-mac = test-host + qa-runtime on macOS (dragon)"
```

### qa-linux
Linux QA gate = test-host + qa-runtime on `runs-on: sandbox` (tier-3 container via
bashy podman) or a `[self-hosted, sdlc, linux]` host.
Requires: test-host qa-runtime
Effects: write
```bash
echo ">> qa-linux = test-host + qa-runtime on Linux (sandbox container)"
```

### qa-win
Windows QA gate = test-host + qa-runtime on `runs-on: [self-hosted, sdlc, windows]`
(puppy). Windows is where RUNTIME (not compile) bugs live — qa-runtime is the one
that matters here (in-process shell/pipe/PTY, real-git).
Requires: test-host qa-runtime
Effects: write
```bash
echo ">> qa-win = test-host + qa-runtime on Windows (puppy)"
```

### release-prod
GATED, tag-driven, byte-promoted. The pipeline is:
1. `git tag vX.Y.Z-dev && git push` → `release.yml` builds the 6-platform artifacts
   as a PRE-RELEASE (stamped/named with the BASE version vX.Y.Z; users' installer
   skips it via `/releases/latest`).
2. Per-OS QA pollers (`scripts/qa-poller.sh` / this repo's README `qa` dag) download
   those exact bytes, run the minimal won't-brick smoke, and on pass create
   `refs/qa/vX.Y.Z/<os>`.
3. Once the required OS set is green, **push the bare `vX.Y.Z` tag**
   (`git tag vX.Y.Z && git push origin vX.Y.Z`) — that fires
   `.github/workflows/promote.yml`, which re-checks the gate, downloads the tested
   pre-release, and publishes the OFFICIAL `vX.Y.Z --latest` from those exact bytes
   (NO rebuild — release.yml only builds `-dev`), then fires the fleet-notify webhook.
   (`gh workflow run promote.yml -f version=vX.Y.Z` is the manual fallback.)
The paired bashy release must match (outpost carries DefaultBashyVersion) — see
docs/cicd-strategy.md + docs/sdlc-deploy-targets-design.md.
Effects: write
```bash
echo ">> prod is a gated, tag-triggered promotion — do NOT auto-run from this dag."
echo ">> gate: refs/qa/<ver>/<os> for the required OS set must be green."
echo ">> then: git tag vX.Y.Z && git push origin vX.Y.Z  (fires promote.yml; byte-promote, no rebuild)."
exit 1   # the operator fires prod by pushing the bare tag, never this dag
```
