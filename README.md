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
.\scripts\build.ps1       # Windows → .\bin\outpost.exe
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
