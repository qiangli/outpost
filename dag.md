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
uname_s=$(bashy uname -s 2>/dev/null || uname -s)
case "$uname_s" in *[Dd]arwin*) os=darwin;; *[Ll]inux*) os=linux;; *) os=windows;; esac
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

### qa-lanes
Drive every required-OS QA lane NOW, instead of waiting for a standing poller to
notice the newest `-dev` tag on its own timer.

The workflow is unchanged: each lane still downloads the PUBLISHED artifact for
its OS, sha256-verifies it, runs the same minimal smoke as `qa`, and authors
`refs/qa/<version>/<os>` on success. Only the TRIGGER moves — from a 15-minute
schedule to this task.

WHY: the scheduled pollers stopped attesting for 21 versions and nothing said
so, because each host ran its own copy of the wrapper and one of them had had
its self-update line deleted. Driving the lanes from the repo removes that
failure mode entirely — there is no per-host copy left to go stale, and a lane
that cannot run FAILS THIS TASK instead of silently producing nothing.

Cheap to re-run: a lane whose ref already exists early-returns ("already
promoted"), so this is a no-op once the version is attested.

Lanes are configured by env, and each maps to the runner that already exists:
  QA_LANES          space-separated (default "darwin windows")
  darwin/linux/…    run here            -> scripts/qa-poller.sh
  <lane>_REMOTE     run over ssh        -> scripts/qa-poller-broker.sh
  linux via podman  no Linux host       -> scripts/qa-poller-podman.sh

A lane needing a remote host takes `<LANE>_REMOTE` and `<LANE>_BASHY`, e.g.
`WINDOWS_REMOTE=<host> WINDOWS_BASHY='C:/path/to/bashy.exe'`. Skipping a
required lane is possible (drop it from QA_LANES) and is NOT recommended: it
does not make the gate pass, it makes the gate absent — `promote` will still
refuse, which is the point.
Effects: write

```bash
set -e
REPO="${OUTPOST_REPO:-qiangli/outpost}"
LANES="${QA_LANES:-darwin windows}"

# Which OS is THIS host? A lane may only run here if it IS our lane. See the
# refusal below for why that check is the whole point of this task.
case "$(bashy uname -s 2>/dev/null || uname -s)" in
  Darwin)                          hostlane=darwin ;;
  Linux)                           hostlane=linux ;;
  Windows_NT|MINGW*|MSYS*|CYGWIN*) hostlane=windows ;;
  *)                               hostlane=unknown ;;
esac

# Newest vX.Y.Z-dev — same awk as `promote` (no sort -V on the target userland).
# Needed HERE so a lane can ask "am I already attested?" BEFORE a runner is
# chosen; the ref is the verdict, and asking first is what lets `promote`
# re-run this task as a dependency without every lane's remote env.
dev=$(bashy git ls-remote --tags "https://github.com/$REPO.git" 2>/dev/null | awk -F/ '
  /refs\/tags\/v[0-9]+\.[0-9]+\.[0-9]+-dev$/ {
    t=$NF; sub(/-dev$/,"",t); sub(/^v/,"",t); split(t,p,".")
    n=p[1]*1000000+p[2]*1000+p[3]; if (n>best) { best=n; tag=$NF }
  } END { print tag }')
[ -n "$dev" ] || { echo "qa-lanes: no vX.Y.Z-dev tag found in $REPO"; exit 1; }
ver="${dev%-dev}"

for lane in $LANES; do
  # NOT `tr 'a-z' 'A-Z'`: tr's ranges are locale-sensitive and return EMPTY
  # under some LC_COLLATE values, which is the exact defect that made a macOS
  # poller call itself the windows lane. Explicit cases cannot do that.
  case "$lane" in
    darwin) up=DARWIN ;;
    windows) up=WINDOWS ;;
    linux) up=LINUX ;;
    *) up=$(printf '%s' "$lane" | awk '{print toupper($0)}') ;;
  esac
  eval "remote=\${${up}_REMOTE:-}"
  eval "rbashy=\${${up}_BASHY:-}"
  eval "rarch=\${${up}_ARCH:-amd64}"

  if bashy gh api "/repos/$REPO/git/ref/qa/$ver/$lane" >/dev/null 2>&1; then
    echo ">> lane $lane: $ver already attested"
    continue
  fi

  if [ -n "$remote" ]; then
    echo ">> lane $lane: over ssh to $remote"
    QA_LANE="$lane" QA_REMOTE="$remote" QA_REMOTE_ARCH="$rarch"       QA_REMOTE_BASHY="${rbashy:-bashy}" QA_POLL_ONCE=1       bashy scripts/qa-poller-broker.sh
  elif [ "$lane" = linux ] && [ "$hostlane" != linux ]; then
    echo ">> lane linux: via podman on this host"
    QA_POLL_ONCE=1 bashy scripts/qa-poller-podman.sh
  elif [ "$lane" != "$hostlane" ]; then
    # THE FAILURE THIS REFUSAL EXISTS FOR — it happened, on v0.14.32.
    # Without it a lane with no <LANE>_REMOTE fell through to the LOCAL
    # poller, which self-identifies by the HOST's OS. So `promote`, which
    # re-runs this task as a dependency WITHOUT any remote env, printed
    #     >> lane windows: on this host
    #     [darwin] v0.14.32 already promoted
    # and exited 0. It announced the windows lane and executed the darwin
    # one. The windows platform was never touched and the task reported
    # success — the exact silent non-attestation this task was built to
    # remove, and which previously went unnoticed for 21 versions.
    echo "qa-lanes: REFUSING lane '$lane' — no ${up}_REMOTE and this host is '$hostlane'." >&2
    echo "  A lane must run on its own OS, or its attestation would be a lie." >&2
    echo "  Set ${up}_REMOTE=<ssh-host> (plus ${up}_BASHY=<abs path> when bashy is" >&2
    echo "  not the first match on that host's PATH, and ${up}_ARCH when not amd64)." >&2
    echo "  Dropping '$lane' from QA_LANES does NOT make the gate pass — it makes" >&2
    echo "  the gate ABSENT, and promote will still refuse. That is the point." >&2
    exit 1
  else
    echo ">> lane $lane: on this host"
    QA_POLL_ONCE=1 bashy scripts/qa-poller.sh
  fi
done
```

### promote
Drive the LAST step of the two-tag release flow: check that every required-OS QA
lane has attested the newest `vX.Y.Z-dev`, then create the bare `vX.Y.Z` tag —
the push is what fires `promote.yml`, which byte-promotes the tested pre-release
and notifies the fleet.

This exists because the chain had no single driver. `release.yml` built, the
standing pollers attested, and `promote.yml` waited — but nothing joined them, so
when a lane stopped attesting nobody noticed for 21 versions. One command with a
visible failure is the fix; it invents no new mechanism and replaces nothing.

`REQUIRED_OS` MUST stay aligned with `promote.yml`'s required set (default
`windows`). Loosening it here would silently turn a real gate into an absent one.
It does NOT run the lanes — the standing pollers do that
(`docs/qa-poller-host-setup.md`); this only reads their verdict, so a missing ref
means "that lane did not attest", never "that lane was skipped".

Pure-bashy like `qa`: no `grep -o`, no `sort -V`.
Requires: qa-lanes
Effects: write

```bash
set -e
REPO="${OUTPOST_REPO:-qiangli/outpost}"
REQUIRED_OS="${REQUIRED_OS:-windows}"

# newest vX.Y.Z-dev (awk, not sort -V — the target userland has neither)
dev=$(bashy git ls-remote --tags "https://github.com/$REPO.git" 2>/dev/null | awk -F/ '
  /refs\/tags\/v[0-9]+\.[0-9]+\.[0-9]+-dev$/ {
    t=$NF; sub(/-dev$/,"",t); sub(/^v/,"",t); split(t,p,".")
    n=p[1]*1000000+p[2]*1000+p[3]; if (n>best) { best=n; tag=$NF }
  } END { print tag }')
[ -n "$dev" ] || { echo "promote: no vX.Y.Z-dev tag found in $REPO"; exit 1; }
ver="${dev%-dev}"
echo "promote: newest pre-release is $dev"

# Already promoted? The bare tag IS the promotion baton, so its existence is the
# answer — do not re-push it (that would re-fire promote.yml for nothing).
if bashy gh api "/repos/$REPO/git/ref/tags/$ver" >/dev/null 2>&1; then
  echo "promote: $ver already promoted"; exit 0
fi

# The gate. A missing ref is a lane that did NOT attest; say which, and stop.
missing=""
for os in $REQUIRED_OS; do
  bashy gh api "/repos/$REPO/git/ref/qa/$ver/$os" >/dev/null 2>&1 || missing="$missing $os"
done
if [ -n "$missing" ]; then
  echo "promote: $ver BLOCKED — no QA attestation from:$missing"
  echo "  the standing poller for that lane has not passed these bytes;"
  echo "  see docs/qa-poller-host-setup.md before overriding anything."
  exit 1
fi
echo "promote: $ver gate green for [$REQUIRED_OS]"

# Point the bare tag at the SAME commit the pre-release was built from, so the
# promoted tag can never name different bytes than the ones QA ran.
sha=$(bashy git ls-remote "https://github.com/$REPO.git" "refs/tags/$dev" | awk '{print $1}' | head -1)
[ -n "$sha" ] || { echo "promote: could not resolve $dev to a commit"; exit 1; }
bashy gh api -X POST "/repos/$REPO/git/refs" -f "ref=refs/tags/$ver" -f "sha=$sha" >/dev/null \
  || { echo "promote: could not create refs/tags/$ver (token needs Contents: write)"; exit 1; }
echo ">> PROMOTED $ver ($sha) — promote.yml will byte-promote and notify the fleet"
```
