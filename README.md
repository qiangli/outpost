# outpost

The home-host agent for [ai.dhnt.io](https://ai.dhnt.io) — one binary per
machine you want to surface through the portal. It registers with a one-time
code, dials back over a secure tunnel, and serves your local apps (HTTP, shell,
desktop, clipboard) so authenticated portal users reach them at
`https://ai.dhnt.io/h/<host>/app/<name>/`. The HTTP server binds loopback only;
the portal is the sole ingress.

Fully open source under the [MIT license](LICENSE) — nothing is hidden, nothing
phones home. The wire protocol is documented inline in
[`internal/agent/portal/`](internal/agent/portal/) (`exchange.go` is the only
handshake); a self-built outpost pairs with the public portal exactly like an
official binary.

## Install

**macOS / Linux** — one-line installer (downloads the matching release binary,
verifies sha256, optionally registers launchd / systemd):

```bash
curl -fsSL https://raw.githubusercontent.com/qiangli/outpost/main/scripts/install.sh | sh
```

**Windows** — PowerShell installer (`Invoke-WebRequest` avoids Mark-of-the-Web,
so SmartScreen does not gate first run):

```powershell
iwr -useb https://raw.githubusercontent.com/qiangli/outpost/main/scripts/install.ps1 | iex
```

Environment overrides (`INSTALL_DIR`, `OUTPOST_VERSION`, `NO_SERVICE`), Linux PAM
auth, Windows Defender notes, and uninstall: [`docs/install.md`](docs/install.md)
(`outpost docs install`).

## Pair with the portal

1. Sign in at <https://ai.dhnt.io/admin/> → **Hosts** → **Generate invite code**.
2. On the home machine:

   ```bash
   outpost register --server https://ai.dhnt.io --code <one-time-code> --name laptop
   outpost start
   ```

`register` exchanges the code for the agent's persistent config; `outpost start`
dials in and serves the local apps. Declare apps with
`MATRIX_APPS="ycode=http://127.0.0.1:8765,jupyter=http://127.0.0.1:8888"` (ycode
at `:8765` is the default). Full flow: [`docs/pairing.md`](docs/pairing.md).

## What outpost serves

- `/app/<name>/*` — reverse-proxy for any HTTP app declared in `MATRIX_APPS`
- `/shell` — admin-tier PTY shell (WebSocket) · `/desktop` — VNC relay ·
  `/clipboard` — clipboard bridge
- `/auth` — credential check against the host OS (or a custom `--auth-url`)

All reached only through the portal — the HTTP server binds `127.0.0.1:<random>`.

## Build from source

Requires Go 1.25+. `go install …@latest` does **not** work — go.mod carries a
sibling-path `replace` for the forked shell runner, which the build scripts
materialize for you.

```bash
git clone https://github.com/qiangli/outpost && cd outpost
./scripts/build.sh                 # macOS / Linux → ./bin/outpost
# Windows: powershell -ExecutionPolicy Bypass -File .\scripts\build.ps1
```

Already have any outpost release? `outpost build` does the whole flow — clone,
sibling bootstrap, build — with only the Go toolchain (no system git / make /
bash). Pin with `outpost build --ref v0.3.0` (tag or sha), then
`outpost upgrade --local <built>`. All build paths, pinning, and cross-compile:
[`docs/building.md`](docs/building.md) (`outpost docs building`).

## Testing a release build (QA)

A QA host verifies a *published* build (no source, no Go — bashy only, which
self-provisions git / coreutils) via the `qa` task in [`dag.md`](dag.md):

```bash
OUTPOST_TEST_VERSION=v1.2.3-dev bashy dag dag.md qa
```

It downloads this host's OS/arch artifact, verifies sha256, and runs a runtime
smoke — the same probe the upgrade worker runs before swapping a live binary.
Standing-poller setup: [`docs/qa-poller-host-setup.md`](docs/qa-poller-host-setup.md).

## Docs

- [`docs/install.md`](docs/install.md) · [`docs/building.md`](docs/building.md) ·
  [`docs/pairing.md`](docs/pairing.md)
- [`docs/settings.md`](docs/settings.md) — every config key ·
  [`docs/mcp.md`](docs/mcp.md) — agent tool surface
- [`docs/remote-podman.md`](docs/remote-podman.md) ·
  [`docs/cluster-gpu.md`](docs/cluster-gpu.md) ·
  [`docs/windows-service.md`](docs/windows-service.md)
- Task graph: [`dag.md`](dag.md) (`bashy dag --list`) · Release notes:
  <https://github.com/qiangli/outpost/releases>

> **Agents working in this repo** (with a local `bashy kb`):
> `bashy kb show outpost-orientation` for the operational map;
> `never-pkill-on-an-outpost-host` and
> `outpost-upgrade-local-no-ops-on-same-commit-rebuilds` for the sharp edges.

PRs welcome.
