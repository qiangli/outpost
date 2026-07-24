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

The `qa` target is the odd one out: it does not build from source — it verifies a
**published** release binary (`OUTPOST_TEST_VERSION=v1.2.3-dev bashy dag dag.md qa`)
and is what the standing QA poller runs on each OS. See its task entry below.

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
BASHY_EXE="${BASHY:-bashy}"
"$BASHY_EXE" go test -short ./...
```

### test-headless
Short tests minus internal/agent/shell — safe in a TTY-less environment.
Effects: read, net

```bash
BASHY_EXE="${BASHY:-bashy}"
"$BASHY_EXE" go test -short $("$BASHY_EXE" go list ./... | grep -v internal/agent/shell)
```

### tidy
go mod tidy + go fmt + go vet.
Effects: write, net

```bash
./scripts/tidy.sh
```

### install
Build then install the binary into `$DHNT_BIN_DIR` (default `~/.local/bin`).
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

### qa
Verify a *published* release build (no source build, no Go — only bashy, which
self-provisions git/coreutils). Downloads `$OUTPOST_TEST_VERSION` for THIS host's
OS/arch, verifies sha256, and runs a MINIMAL smoke — only enough to guarantee a
fleet rollout of these exact bytes won't brick a registered host: the binary
executes, self-reports the expected version (the same probe the upgrade worker runs
before it swaps the live binary), and its in-process shell + real-git surfaces
answer. No `/tmp`, no `grep -o`/`sort -V` (bashy = the target userland has neither)
— pure-bashy, so it runs identically on macOS, Linux, and Windows. `OTEL_*` is
honored, so a failure reports to the dev conductor's telemetry backend. This is what
the standing QA poller runs (`docs/qa-poller-host-setup.md`).
Effects: write

```bash
set -e
REPO="${OUTPOST_REPO:-qiangli/outpost}"
VER="${OUTPOST_TEST_VERSION:?set OUTPOST_TEST_VERSION to the tag to test, e.g. v1.2.3-dev}"
# Download FROM the release tag (VER, e.g. v1.2.3-dev) but the asset is NAMED with
# the base version (bytes are stamped base — see release.yml byte-promotion).
BASEV="${VER%%-*}"
os=$(bashy uname -s | tr 'A-Z' 'a-z'); case "$os" in *darwin*) os=darwin;; *linux*) os=linux;; *) os=windows;; esac
arch=$(bashy uname -m); case "$arch" in arm64|aarch64) arch=arm64;; x86_64|amd64) arch=amd64;; esac
ext=""; [ "$os" = windows ] && ext=.exe
base="https://github.com/${REPO}/releases/download/${VER}"
asset="outpost-${BASEV}-${os}-${arch}${ext}"
d=".qa"; bashy mkdir -p "$d"       # cwd-local temp — /tmp isn't guaranteed on Windows
echo ">> QA ${VER} on ${os}/${arch} — ${asset}"
bashy curl -fsSL -o "$d/${asset}" "${base}/${asset}"
if bashy curl -fsSL -o "$d/out.sha256" "${base}/outpost-${BASEV}-${os}-${arch}.sha256" 2>/dev/null; then
  # the .sha256 sidecar is "<sha>  <filename>"; extract with awk (no grep -o).
  # Fail closed: a missing/empty sha is a hard failure — never run unverified bytes.
  want=$(awk '{print $1}' "$d/out.sha256" | head -1)
  got=$(bashy sha256sum "$d/${asset}" | awk '{print $1}' | head -1)
  { [ -n "$want" ] && [ "$want" = "$got" ]; } || { echo "FAIL sha256 (want=$want got=$got)"; exit 1; }
  echo ">> sha256 verified"
fi
chmod +x "$d/${asset}" 2>/dev/null || true
BIN="$d/${asset}"
# 1. it EXECUTES and self-reports the expected version (== the upgrade worker's Probe;
#    a binary that fails this is exactly what bricks a host on swap+re-exec).
vout=$("$BIN" version | head -1); echo "   $vout"
case "$vout" in *"$BASEV"*) ;; *) echo "FAIL: version stamp is not $BASEV ($vout)"; exit 1;; esac
# 2. the in-process shell engine runs (the /shell + /ssh surface a live host serves).
[ "$("$BIN" shell -c 'echo runtime-ok')" = "runtime-ok" ] || { echo "FAIL: shell -c"; exit 1; }
# 3. real-git surface resolves (Windows-without-system-git relies on it).
"$BIN" git --version >/dev/null 2>&1 || { echo "FAIL: git surface"; exit 1; }
echo ">> QA PASS ${VER} ${os}/${arch}"
```
