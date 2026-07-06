# outpost

The home-host agent for [ai.dhnt.io](https://ai.dhnt.io).

One outpost binary runs on each machine you want to surface through the
portal. It registers with the portal using a one-time code, dials back
over a secure tunnel, and serves the local apps (HTTP, shell, desktop,
clipboard) so that authenticated portal users can reach them through
`https://ai.dhnt.io/h/<host>/app/<name>/`.

## Install

**macOS / Linux** — one-line installer (downloads the matching release binary, verifies sha256, optionally registers launchd / systemd):

```bash
curl -fsSL https://raw.githubusercontent.com/qiangli/outpost/main/scripts/install.sh | sh
```

**Windows** — PowerShell installer (`Invoke-WebRequest` avoids Mark-of-the-Web, so SmartScreen does not gate first run):

```powershell
iwr -useb https://raw.githubusercontent.com/qiangli/outpost/main/scripts/install.ps1 | iex
```

**From source** — if you have Go 1.25+ (note: `go install …@latest` does
not work here — go.mod carries a sibling-path `replace` for the forked
shell runner, which the build scripts materialize for you):

```bash
git clone https://github.com/qiangli/outpost.git && cd outpost
./scripts/build.sh        # macOS / Linux → ./bin/outpost
```

```powershell
git clone https://github.com/qiangli/outpost.git; cd outpost
powershell -ExecutionPolicy Bypass -File .\scripts\build.ps1   # Windows → .\bin\outpost.exe
```

Already have any outpost release installed? `outpost build` rebuilds
from GitHub source in one command — no system git needed. See
[`docs/building.md`](docs/building.md) (or `outpost docs building`) for
all build paths, tag/commit pinning, and cross-compilation.

See `outpost docs install` (or [`docs/install.md`](docs/install.md)) for the full guide: environment overrides (`INSTALL_DIR`, `OUTPOST_VERSION`, `NO_SERVICE`), Linux PAM auth via `CGO_ENABLED=1`, Windows Defender notes, and uninstall steps.

## Pair with the portal

1. Sign in at <https://ai.dhnt.io/admin/>, open **Hosts**, click
   **Generate invite code**.
2. On the home machine:

   ```bash
   outpost register \
     --server https://ai.dhnt.io \
     --code   <one-time-code> \
     --name   laptop

   outpost start
   ```

`register` exchanges the code for the agent's persistent config and
saves it to the default user-config path. The portal tells the agent
which transport to use; `outpost start` then dials in and starts the
local HTTP server.

By default ycode at `http://127.0.0.1:8765` is registered as an app;
declare more with:

```bash
MATRIX_APPS="ycode=http://127.0.0.1:8765,jupyter=http://127.0.0.1:8888" \
  outpost start
```

## What outpost serves

- `/app/<name>/*` — reverse-proxies any HTTP app you declare in
  `MATRIX_APPS`.
- `/shell` — admin-tier PTY-wrapped shell (WebSocket).
- `/desktop` — admin-tier VNC relay (WebSocket).
- `/clipboard` — clipboard bridge.
- `/auth` — credential check against the host OS by default, or against
  a custom `--auth-url` endpoint for app-level user lists.

All of these are reached only through the portal — outpost binds its
HTTP server to loopback (`127.0.0.1:<random>`).

## Build from source

outpost is **fully open source** under the [MIT license](LICENSE). You
don't have to trust the binaries we publish — clone the repo, read every
line of code that talks to the portal, build your own binary, and pair
that with [ai.dhnt.io](https://ai.dhnt.io). Nothing is hidden, nothing
phones home behind your back.

```bash
git clone https://github.com/qiangli/outpost
cd outpost
./scripts/build.sh   # → ./bin/outpost
./bin/outpost register --server https://ai.dhnt.io --code <code> --name <host>
./bin/outpost start
```

Requires Go 1.25+. `./scripts/build.sh` (and its Windows twin
`.\scripts\build.ps1`) first materializes the `../sh` sibling the
go.mod `replace` directive needs, then runs `go build ./cmd/outpost`
with the version + commit baked into the binary so `outpost version`
reports a build you can trace back to a git SHA. Already have outpost
installed? `outpost build` does the whole flow — clone from GitHub,
sibling bootstrap, build — without system git, make, bash, or
coreutils; only the Go toolchain. Pin a version with
`outpost build --ref v0.3.0` (tag) or `--ref <sha>` (any commit), then
swap it in with `outpost upgrade --local <built>`. The full guide is
[`docs/building.md`](docs/building.md) (`outpost docs building`); see
[`docs/install.md`](docs/install.md) for the CGO-enabled recipe needed
for Linux PAM auth (`CGO_ENABLED=1` + `libpam-dev`) and Windows
Defender notes.

**Forking, modifying, contributing** — outpost has no proprietary
hooks. The wire protocol with cloudbox is documented inline in
[`internal/agent/portal/`](internal/agent/portal/) (the `exchange.go`
round-trip is the only handshake), and every config key is in
[`docs/settings.md`](docs/settings.md). A modified outpost that obeys
the same protocol will pair with the public portal the same way an
official binary does. PRs welcome.

Release notes: <https://github.com/qiangli/outpost/releases>.

## Testing a release build (QA) — this section is a `bashy dag`

A QA host verifies a *published* build (no source build, no Go — only bashy, which
self-provisions git/coreutils) with:

```bash
OUTPOST_TEST_VERSION=v1.2.3-dev bashy dag README.md qa
```

It downloads the release artifact for **this** host's OS/arch, verifies sha256, and
runs a runtime smoke — the same steps a human would follow, kept executable so the
docs can't drift from what CI runs. `OTEL_*` is honored, so a failure is reported
to the dev conductor's telemetry backend (it dispatches the fleet to fix it).

## Tasks

### qa
Download `$OUTPOST_TEST_VERSION` for this OS/arch, verify sha256, smoke it.
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
echo ">> QA ${VER} on ${os}/${arch} — ${asset}"
bashy curl -fsSL -o "/tmp/${asset}" "${base}/${asset}"
if bashy curl -fsSL -o "/tmp/out.sha256" "${base}/outpost-${BASEV}-${os}-${arch}.sha256" 2>/dev/null; then
  # the .sha256 sidecar is "<sha>  <filename>"; extract with awk (bashy's pure-Go
  # grep has no -o). Fail closed: a missing/empty sha is a hard failure.
  want=$(awk '{print $1}' "/tmp/out.sha256" | head -1)
  got=$(bashy sha256sum "/tmp/${asset}" | awk '{print $1}' | head -1)
  { [ -n "$want" ] && [ "$want" = "$got" ]; } || { echo "FAIL sha256 (want=$want got=$got)"; exit 1; }
  echo ">> sha256 verified"
fi
chmod +x "/tmp/${asset}" 2>/dev/null || true
BIN="/tmp/${asset}"
"$BIN" version | head -1
[ "$("$BIN" shell -c 'echo runtime-ok')" = "runtime-ok" ] || { echo "FAIL: shell -c"; exit 1; }
[ "$("$BIN" shell -c 'printf "a\nb\n" | grep b')" = "b" ] || { echo "FAIL: in-process pipe"; exit 1; }
"$BIN" git --version >/dev/null 2>&1 || { echo "FAIL: git surface"; exit 1; }
echo ">> QA PASS ${VER} ${os}/${arch}"
```
