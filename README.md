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

**From source** — if you have Go 1.25+:

```bash
go install github.com/qiangli/outpost/cmd/outpost@latest
```

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

```bash
go build ./cmd/outpost
```

Requires Go 1.25+. See [`docs/install.md`](docs/install.md) for the CGO-enabled recipe needed for Linux PAM auth.

Release notes: <https://github.com/qiangli/outpost/releases>.
