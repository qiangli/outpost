# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

`outpost` is the home-host agent for the cloud portal at `ai.dhnt.io` (the **cloudbox** service). One binary runs on each machine the user wants to surface through the portal: it `register`s once with a one-time code, then `start` dials back through the matrix tunnel and serves local apps (HTTP reverse-proxy, PTY shell, VNC desktop, clipboard, /auth) so portal users can reach them at `https://ai.dhnt.io/matrix/h/<host>/app/<name>/`.

The local HTTP server binds loopback only — the cloud reaches it strictly through the matrix tunnel.

## Common commands

Requires Go 1.25+ (see `go.mod`). Note the two sibling-path replaces in `go.mod`: `replace mvdan.cc/sh/v3 => ../sh` (the shell runner depends on a fork) and `replace github.com/qiangli/coreutils => ../coreutils` (the pure-Go git client behind `outpost git`, plus — over time — the rest of the agent userland). The sh fork additionally implements `disown` / `kill` / `nohup` / `setsid` as builtins (upstream has them only as declarations or not at all), which is what lets `nohup ... &` survive a closed SSH session in the matrix shell. The fork also ships `mvdan.cc/sh/v3/interactive` — a reusable read-edit-execute loop wrapping `ergochat/readline` around `interp.Runner`, originally extracted from `cmd/bashy`; this is what gives the matrix shell + `/ssh` arrow-key history, cursor editing, and Ctrl-R reverse search that upstream `parser.Interactive` does not provide.

The sibling-path replaces resolve in two contexts: inside the dhnt umbrella they point at the `dhnt/sh` and `dhnt/coreutils` submodules; standalone, run `./scripts/bootstrap-siblings.sh` to clone each into `../<name>` at the SHA pinned in `.sibling-pins`. CI runs the bootstrap automatically. The bootstrap script prefers `outpost git` when an outpost is on PATH (so a Windows machine with only outpost + go installed can self-rebuild) and falls back to system `git` otherwise.

```bash
# Build scripts (no Makefile — bash scripts under scripts/ are the canonical entry points)
./scripts/build.sh           # → ./bin/outpost
./scripts/build-all.sh       # cross-compile darwin/linux/windows × amd64/arm64
./scripts/install-bin.sh     # build + install to $HOME/bin (override via INSTALL_DIR)
./scripts/tidy.sh            # go mod tidy + go fmt ./... + go vet ./...
./scripts/clean.sh           # rm -rf ./bin
./scripts/bootstrap-siblings.sh   # materialize ../sh from .sibling-pins (idempotent)

# Rebuild outpost from outpost (zero system-git, zero make — only Go toolchain required):
outpost git clone https://github.com/qiangli/outpost.git
cd outpost
outpost shell ./scripts/bootstrap-siblings.sh
outpost shell ./scripts/build.sh
# → ./bin/outpost

# Tests (no test wrapper script). Test files live under:
#   internal/agent/{auth,apps,clipboard,ssh,hostkey,tunnel,outbound}_test.go
#   internal/agent/portal/exchange_test.go
#   internal/agent/conf/{file,url}_test.go
#   internal/agent/admincore/networking_test.go
#   internal/agent/adminui/{adminui,e2e,suggestions,builtins,cluster,login_limiter}_test.go
#   internal/agent/mcpapi/mcpapi_test.go        — end-to-end MCP protocol roundtrip
#   cmd/outpost/docs_test.go                    — drift detector for embedded docs
go test ./...
go test ./internal/agent -run TestAuth
go test ./internal/agent/adminui -run TestE2E
go test ./internal/agent/mcpapi                 # initialize → list tools → call

# Run from source
go run ./cmd/outpost register --server https://ai.dhnt.io --code <code> --name <host>
go run ./cmd/outpost start
go run ./cmd/outpost stop
go run ./cmd/outpost status                # paired-yet + builtins + apps + outbound
go run ./cmd/outpost docs settings         # embedded reference (every persisted setting)

# MCP-backed CLI subcommands (drive the running daemon via /mcp/*):
go run ./cmd/outpost apps {list,add,rm,rotate-token,suggest}
go run ./cmd/outpost builtins {show,set --ssh=on/off …}
go run ./cmd/outpost config {show,set --admin-addr 0.0.0.0:17777 …}
go run ./cmd/outpost outbound {list,add,connect,disconnect,rm,suggest}
go run ./cmd/outpost cluster {kubeconfig,userkubeconfig,set,clear,init}
go run ./cmd/outpost mcp {endpoint,rotate-token}
go run ./cmd/outpost pool status           # local LLM-pool participation snapshot
go run ./cmd/outpost depart                # tell cloudbox we're going offline (clean shutdown)
go run ./cmd/outpost kubectl -- get nodes  # auto-fetches a per-user kubeconfig, then execs kubectl
go run ./cmd/outpost upgrade [--local PATH | --from URL]   # operator-driven self-upgrade
go run ./cmd/outpost rollback              # swap <binary>.previous back over the live binary
go run ./cmd/outpost remote {login,logout,list} <name>     # cached creds for LAN deploy targets
go run ./cmd/outpost {jobs,fg <pid>,bg <pid>,kill <pid> [SIG]}  # manage matrix-shell detached jobs
go run ./cmd/outpost run -- <cmd> [args...]                # submit as per-user launchd agent (macOS)
go run ./cmd/outpost shell [-c "cmd"]                      # local in-process bash (same engine as matrix shell)
go run ./cmd/outpost {restart,unpair}

# Embedded git client (no system git required — Windows-friendly):
go run ./cmd/outpost git clone https://github.com/qiangli/outpost
go run ./cmd/outpost git {status,log,diff,branch,show,remote,fetch,pull}
go run ./cmd/outpost git {add,commit,checkout,push,init}
go run ./cmd/outpost git {merge,merge-base,rev-list,config,tag,reset,rm,ls-files,blame,grep}

# Companion binaries (separate build targets, separate concerns):
go run ./cmd/outpost-vk -kubeconfig ~/.kube/config -node <name>  # standalone virtual-kubelet PoC (vkpodman)
# Phase-3 CNI plugin (kubelet exec, not a daemon) — source lives at
# internal/agent/runtime/image/cni/ and is compiled inside the runtime
# container by the multi-stage Dockerfile. No standalone build target.
outpost cluster build-runtime                                    # rebuild the runtime container image from embedded sources

# Client-side helpers (target a paired host from a different machine):
go run ./cmd/outpost connect <host>        # mirrors the web "Connect" button (caches matrix_elev cookie)
go run ./cmd/outpost ssh-proxy <host>      # SSH ProxyCommand (used by ~/.ssh/config stanzas)
go run ./cmd/outpost ssh-config            # emit ~/.ssh/config stanzas for visible hosts
go run ./cmd/outpost ssh <host> [cmd...]   # drop-in ssh; LAN-direct when possible, cloudbox fallback
go run ./cmd/outpost ssh {list,add,show,rm,exec,tunnel,sftp,ls,get,put}  # SSH target tree
go run ./cmd/outpost sshd [--addr :2222]   # standalone LAN SSH server (drop-in sshd; no daemon/pairing/cloudbox needed)
go run ./cmd/outpost scp [user@]host:src dst         # SFTP-backed copy
go run ./cmd/outpost scp --safe [--keep-previous] src host:dst  # amfid-safe binary delivery (atomic posix-rename)
go run ./cmd/outpost shasum [user@]host:path         # sha256 of a remote file (shasum -a 256 format)
go run ./cmd/outpost reach [user@]host               # lan|cloudbox|offline classifier (exit 0|10|20)

# Discovery + peer-assisted repair (Wave 3A):
go run ./cmd/outpost scan                            # mDNS browse for outposts on the LAN
go run ./cmd/outpost peers {list,route-to,history,predicted,help-mint-invite}
go run ./cmd/outpost repair {cloudbox-url,binary,register,remote-binary}  # recover from a broken peer
go run ./cmd/outpost version                         # build provenance (commit, dirty flag, Go version)
```

`outpost start` no longer requires `register` first — on an unpaired host it brings up the admin UI and waits. `register` still exists for installer scripts and for users who want the whole pairing in one CLI invocation; `register --yes` (or answering "yes" to the prompt) re-execs the binary as a detached background process — see `cmd/outpost/main.go:execSelfStart` and `detach_unix.go` / `detach_windows.go`.

## Architecture

### Process layout

`cmd/outpost/main.go` wires a sprawling CLI surface (see "Common commands" above). `start` always launches the **admin / agent-tool loopback listener** (default `127.0.0.1:17777`, override via `$OUTPOST_ADMIN_ADDR` or `--admin-addr`). The listener hosts three things on disjoint path prefixes:

```
127.0.0.1:17777
├── /                  SPA (humans, session-cookie auth)
├── /api/*             admin REST API (humans, session-cookie auth)
├── /mcp/*             MCP streamable HTTP server (agents, bearer-token auth)
├── /_periscope/*      per-app provisioning relay (per-app bearer)
└── /healthz
```

The listener is *never* advertised through the matrix tunnel — it is local-machine only. Two auth schemes coexist by path-prefix isolation (the session-cookie middleware is mounted only on the `/api/*` group; the bearer middleware lives inside `mcpapi.Server.bearerAuth` wrapping `/mcp/*`). They cannot leak into each other.

After the listener is up `start` looks at the merged config:

- **Unconfigured** (`AgentName == ""`): print the admin URL + MCP endpoint + bearer token, block on signal/restart. No matrix tunnel, no main loopback server. The user opens the admin URL (or uses MCP / CLI / `outpost register`) to pair; the operation triggers a self-restart.
- **Configured**: continue as before — bind a random loopback port for the main `gin.Engine` (`agent.RegisterRoutes`), build an embedded matrix-tunnel client (`agent.NewTunnel`) and dial `cfg.ServerAddr:ServerPort` with one TCP proxy pointing at the loopback port. All three (admin listener, main server, matrix tunnel) run in the same `errgroup`; cancelling the context shuts them all down.

`start` refuses to boot if another outpost owns the pidfile at `<UserCacheDir>/outpost/outpost.pid` (the matrix-tunnel `RemotePort` is fixed, so two instances would fight over the same slot). `stop` reads that pidfile, SIGTERMs, then SIGKILLs after 5 s.

### Self-restart for tunnel/identity changes

The matrix tunnel is immutable after `NewTunnel`, and the built-in routes (`/shell`, `/desktop`, `/clipboard`) are mounted conditionally at boot. So any save that changes pairing, server URL, agent name, networking binds, or a built-in toggle triggers a binary self-restart — regardless of which surface (SPA / MCP / CLI) initiated it. The flow:

1. Surface handler (`adminui.handleX` / MCP tool / CLI subcommand) calls into `admincore.Server.X(...)`.
2. `admincore` writes the new config (`conf.SaveFile`), mutates live state where applicable, and calls `Server.ScheduleRestart()` — a 1-s debounced timer that collapses rapid toggle flips into a single restart.
3. After the debounce fires, the `restartFn` closure threaded from `main.go` runs: it cancels the errgroup context (so all listeners drain).
4. `g.Wait()` returns; the parent clears the pidfile (so the child can claim it), `execSelfStart`s a detached child, and exits.
5. The deferred `removePidFile` becomes a no-op via an `atomic.Bool` flag — without that, the parent would race-delete the child's freshly-written pidfile.

Custom-app add/edit/remove do *not* restart — `AppRegistry` is concurrency-safe, so the live registry is mutated in place. Outbound mount add/remove/connect/disconnect also stays live (it only touches `OutboundManager`). Restart triggers are: pairing/identity changes, server URL, any built-in toggle (`/shell`, `/desktop`, `/clipboard`, `/ssh`, `sftp` subsystem, plus the podman/ollama built-in proxy apps — those last two register into `AppRegistry` from boot), networking binds (`local_addr` / `vnc_addr` / `admin_addr`), and the `admin_users` allowlist (read once at boot into `AdminSet`).

### Config layering

`internal/agent/conf/`:

- `conf.Load()` reads env vars (`AGENT_*`, `MATRIX_*` — including `AGENT_ADDR`, `AGENT_VNC_ADDR`, `OUTPOST_ADMIN_ADDR`, `MATRIX_SERVER_ADDR`, `MATRIX_SERVER_PORT`, `MATRIX_TOKEN`, `MATRIX_PROTOCOL`, `MATRIX_REMOTE_PORT`, `MATRIX_APPS`, `MATRIX_ADMIN_USERS`, `MATRIX_AUTH_URL`). It deliberately does NOT apply hardcoded defaults — that would mask file lookups. Empty env = empty struct field.
- `conf.LoadFile(path)` reads the JSON written by `register` / the admin UI / MCP / CLI (default path: `<UserConfigDir>/matrix/agent.json` — XDG-aware).
- `start` layers in this precedence order: **CLI flag > env var > FileConfig > hardcoded default** (`conf.Default*` constants). Defaults are applied last so an empty env can fall through to the file value. The portal-returned `Protocol`/`Token`/`RemotePort`/`ServerAddr`/`ServerPort` come from the file and win over env when present.
- `FileConfig.Apps` (structured `[]AppConfig`) is the source of truth once it is present — even an empty slice wins over `MATRIX_APPS`. The legacy env path is still consulted when `fc.Apps == nil` (configs written before the admin UI shipped). Built-in toggles use `*bool` so a missing JSON key on an old config defaults to enabled; read via `fc.ShellOn() / DesktopOn() / ClipboardOn()`.
- Beyond pairing + apps + builtins, `FileConfig` now persists: `local_addr`, `vnc_addr`, `admin_addr`, `admin_users` (the env-only `$MATRIX_ADMIN_USERS` predecessor), `ssh_allow_remote_forward`, `ssh_allow_agent_forward`, `ssh_forward_sockets`, `client_only`, `outbound[]`, `cluster.{enabled,api_url,token,ca,node_name}`, plus two daemon-internal secrets: `admin_session_key` (HMAC for SPA cookies) and `mcp_bearer_token` (auth for `/mcp/*`). Auto-generated on first boot via `conf.EnsureAdminSessionKey` / `conf.EnsureMCPBearerToken`.
- See `docs/settings.md` (also reachable via `outpost docs settings`) for the canonical inventory: every persistable field, its file key, CLI flag, UI location, MCP tool, and side-effect class (Restart / Live / Boot-only).

### admincore — shared business-logic layer (`internal/agent/admincore/`)

Every configuration operation outpost exposes converges on `admincore`. The package is protocol-agnostic — no HTTP, no JSON-RPC, no cobra. It owns:

- `Server` struct holding the FileConfig serialization mutex, the restart-debounce timer, and the cached `BuiltinDetector` (podman/ollama probes).
- Mutation methods one-per-domain: `Pair`, `Unpair`, `SetBuiltins`, `UpsertApp` / `DeleteApp` / `RotateProvisioningToken`, `UpsertOutbound` / `DeleteOutbound` / `ConnectOutbound` / `DisconnectOutbound`, `SetKubeconfig` / `ClearKubeconfig`, `SetNetworking`, `ScheduleRestart`.
- Read-only views: `Status`, `SafeView` (redacted FileConfig), `ListApps`, `ListOutbound`, `AppSuggestions`, `OutboundSuggestions`.
- Validation: `ValidateApp`, `ValidateOutbound`, `normalizePathPrefix`, plus a shared `reservedNames` set (`api`, `static`, `healthz`, `index.html`, `app`, `mcp`, `_periscope`) that all three surfaces enforce identically.
- Typed errors: `*APIError{Status, Msg}`; HTTP layers map `Status` to gin codes, MCP maps to `CallToolResult.IsError=true` text content. Plain `error` becomes 500 / JSON-RPC error.

`main.go` constructs ONE `*admincore.Server` and threads it into both `adminui.New` and `mcpapi.New`. The shared instance is what guarantees the SPA / MCP / CLI can't drift: same mutex serializing saves, same restart-debounce timer collapsing rapid flips, same `AppRegistry` / `OutboundManager` for live mutations.

### Admin UI HTTP layer (`internal/agent/adminui/`)

After the admincore extraction, this package is just the human-facing thin shell:
- `server.go` — listener + gin engine + Serve(ctx). Exposes `Engine()` so `cmd/outpost/main.go` can mount the MCP handler on `/mcp/*` of the same engine.
- `handlers.go` — one HTTP handler per `admincore` method. Each one binds JSON, calls the core method, and renders `respondError(c, err)` which uses `admincore.APIError.HTTPStatus()` for status code mapping.
- `mcp.go` — `GET /api/mcp/credentials` and `POST /api/mcp/token/rotate` — the SPA's "Agent tool credentials" surface.
- `sessions.go` — HMAC-signed cookies (1 h TTL, persisted key in `FileConfig.AdminSessionKey`).
- `middleware.go` — `requireSession`. Skips the gate while `AgentName == ""` (no paired identity to protect yet) when the listener is loopback-only; on a LAN bind it's always-on.
- `login_limiter.go` — per-IP token bucket on `POST /api/login` (default 5 burst / 12 s refill).
- `ui.go` + `ui/index.html` — embedded vanilla-JS SPA via `//go:embed ui`. The Pair tab carries three fieldsets: pairing, MCP credentials (with reveal / copy / rotate), and Advanced settings (networking + admin_users + SSH-advanced toggles + ssh_forward_sockets). Built-in toggle rows render the canonical `agent.json` key as a small monospace `.key-hint` badge next to each label.

### MCP server (`internal/agent/mcpapi/`)

Mounted on the same gin engine at `/mcp/*` using `github.com/modelcontextprotocol/go-sdk` (the official Anthropic SDK; pinned in `go.mod`).

- `server.go` — wraps `mcp.NewStreamableHTTPHandler` with constant-time bearer-token middleware (`bearerAuth`). Token lives in `FileConfig.MCPBearerToken`; the value is in-memory swappable via `Server.Rotate()` so the SPA's "Rotate" button and the `outpost_rotate_mcp_token` tool both update the live state atomically.
- `tools.go` registers Phase-1 parity tools across files `tools_{pair,builtins,networking,apps,outbound,cluster,lifecycle}.go`. Every tool delegates to one `admincore.Server` method.
- `resources.go` — `outpost://status`, `outpost://config`, `outpost://apps`, `outpost://outbound`. Read-only JSON snapshots; same shapes adminui's `/api/*` endpoints return.
- `mcpapi_test.go` drives the full protocol end-to-end against an `httptest.Server`: initialize → list tools (asserts every parity tool is registered) → read resource → call tool. Also covers no-header / wrong-scheme / wrong-token / valid-token paths.

Tool naming uses verb-noun (`outpost_upsert_app`, `outpost_delete_outbound`); CLI subcommands use Unix conventions (`apps add`, `outbound rm`). The mismatch is deliberate — the audiences differ — and `docs/settings.md` documents the mapping.

### CLI as MCP client (`cmd/outpost/`)

`outpost apps`, `outpost builtins`, `outpost config`, `outpost outbound suggest`, `outpost cluster set/clear`, `outpost status`, `outpost restart`, `outpost unpair`, `outpost mcp rotate-token` are all thin MCP clients. The shared plumbing is in `cmd/outpost/mcpclient.go`:

- `dialMCP(ctx)` loads `FileConfig.MCPBearerToken` from disk (mode 0600 — same OS user) and opens a `mcp.StreamableClientTransport` against `http://127.0.0.1:17777/mcp` (or `$OUTPOST_ADMIN_ADDR` if set).
- `callTool(ctx, name, args, out)` and `readResource(ctx, uri, out)` wrap the SDK with JSON round-trip into typed Go structs.
- Connection-refused surfaces as "outpost daemon not running — run `outpost start`" rather than a raw net error.

A few subcommands (`apps add`, `apps rm`, plus the legacy `register` flow) accept `--offline` to bypass MCP and mutate the on-disk `FileConfig` directly via a one-shot `admincore.Server`. Useful for installer scripts that provision a host before the daemon is started for the first time.

The pre-MCP `outbound login/logout/list/add/connect/disconnect/rm` family still uses the session-cookie REST path (`adminClient` in `cmd/outpost/outbound.go`); the new `outbound suggest` uses MCP. Both auth shapes coexist.

### Embedded operator docs (`cmd/outpost/embedded_docs/`, `cmd/outpost/docs.go`)

The canonical operator references live in `docs/<topic>.md` and are mirrored byte-for-byte into `cmd/outpost/embedded_docs/<topic>.md` (the `go:embed` directive can't traverse `..` out of the package). `outpost docs [topic]` walks `embedded_docs/` via `//go:embed all:embedded_docs`. Topics are curated in `docsManifest`. `cmd/outpost/docs_test.go` fails the build if the two copies drift — re-sync with `cp docs/<topic>.md cmd/outpost/embedded_docs/<topic>.md`.

Current topics: `settings` (full inventory of every persisted field), `mcp` (setup + tool catalog + `.mcp.json` snippet).

### Routes (`internal/agent/routes.go`)

All mounted at root:
- `GET /healthz`
- `GET /apps` — returns `{agent, apps:[AppEntry...], builtins:{shell,desktop,clipboard,ssh,sftp}}`. Each `AppEntry` is `{name, scheme, require_login, index_path}`. The `builtins` map tells cloudbox which built-in routes this outpost actually mounted, so the portal can hide disabled tiles. `sftp` is the per-toggle SFTP-subsystem entry (it's nested under the `/ssh` server but advertised separately so cloudbox can show "scp/sftp supported" independently). Older outposts omit `builtins`; cloudbox treats that as legacy "all on".
- `POST /auth` — credential check (see Auth below)
- `GET /shell` — WebSocket PTY (binary frames = bytes, text frame `{"type":"size",...}` = resize)
- `GET /desktop` — WebSocket ↔ TCP VNC relay (`--vnc-addr`, default `127.0.0.1:5900`)
- `GET|POST /clipboard` — pbpaste/pbcopy bridge (works around RFB clipboard quirks)
- `GET /ssh` — WebSocket wrapped as a `net.Conn` and fed to an in-process `golang.org/x/crypto/ssh` server (see SSH section). Accepts the `sftp` subsystem channel when `FileConfig.SFTPEnabled` (default on); rejects everything else.
- `POST /admin/upgrade` — cloudbox-pushed self-upgrade. Only mounts on paired hosts (the route helper is called from `RegisterRoutes` only when `deps.AccessToken != ""` *and* `deps.MountUpgradeRoute != nil`). Body is `upgrade.Envelope{release_id, url, sha256, commit, min_from?}`. **No bearer at the HTTP layer** — the route trusts the matrix tunnel as the auth boundary, same model `/apps` and `/healthz` already use. The earlier design (Bearer `<fc.Token>`) didn't survive contact with real deployments: prod cloudbox often runs with an empty `MatrixToken` and outposts paired against it had `fc.Token == ""`, so the route never mounted. The trust path now relies on three layers: the main HTTP server binds 127.0.0.1 only (so cloudbox-via-tunnel is the only reachable caller), the `auto_upgrade` toggle is the operator's opt-out, and the sha256 + envelope.commit + Probe checks gate what the worker actually does. The handler dispatches to `upgrade.Worker.Apply` which runs StageFromURL → Probe → hardlink `<binary>.previous` → atomic rename → `ScheduleRestart`, emitting one JSONL line to `<cacheDir>/outpost/upgrade.log` per phase. Status codes: 202 accepted / 200 replay / 409 in_flight / 304 same_commit / 403 disabled / 412 min_from / 400 bad envelope. See `docs/settings.md` ("Cloudbox-pushed upgrade flow") for the wire contract; the cloudbox sender is the only client.
- `Any /app/:name/*p` — `httputil.ReverseProxy` to the URL registered under that name

`GET /apps`' `builtins` map covers `shell|desktop|clipboard|ssh|sftp`; `*bool` toggles in `FileConfig` (`ShellEnabled`, `DesktopEnabled`, `ClipboardEnabled`, `SSHEnabled`, `SFTPEnabled`) default to enabled when absent for backwards-compat with old configs.

### Apps

`AppRegistry` (in `internal/agent/apps.go`) holds `name → *url.URL` plus per-app `httputil.ReverseProxy` instances and per-app `AppMeta{RequireLogin, LANOnlyPaths, IndexPath}`. Concurrency-safe via `sync.RWMutex` — admin handlers `Register`/`Unregister` at runtime without touching the tunnel. `RegisterFromConfig(AppConfig)` is the helper that registers based on `AppConfig.Scheme`:

- `http`/`https` — TCP target built from `Host:Port` (Host defaults to `127.0.0.1`).
- `unix`/`npipe` — socket-backed. The registry stores a synthetic `http://socket` URL and a per-app `http.Transport` whose `DialContext` dials the local socket (`internal/agent/dialer{,_other,_windows}.go`). Lets an outpost front `docker.sock` / `podman.sock` / `\\.\pipe\docker_engine` without a TCP bind. HTTP/1.1 Upgrade and websockets still work because `httputil.ReverseProxy` hijacks the conn through this transport the same way it does for the default one.
- `tcp` — raw TCP target at `Host:Port` (e.g. `127.0.0.1:22` or `127.0.0.1:5432`). The `/app/:name/*p` handler doesn't run the reverse proxy for these; it accepts a WebSocket upgrade and byte-splices to the TCP target via `serveTCPBridge`. Paired with a `tcp`-scheme `OutboundConfig` on a peer outpost (see "Outbound mounts"). HTTP-mode and TCP-mode names are mutually exclusive — re-registering a name under a different scheme automatically clears the old mode.

Disabled entries are skipped so the admin UI can keep them around without proxying. Seeded by `buildAppRegistry` in `main.go` from `fc.Apps` when structured config is present, else from `MATRIX_APPS="name1=url1,name2=url2"`, falling back to `ycode → http://127.0.0.1:8765` when both are absent. Path rewrite uses `singleJoin` to strip `/app/<name>` cleanly. `Entries()` returns `[]AppEntry{Name, Scheme, RequireLogin, IndexPath}` for `GET /apps`.

### Per-app access control (`ProxyTo` gate in `apps.go`)

The legacy `guest|user|admin` tier was replaced by three orthogonal knobs on `AppConfig` (commit fbe403f). `LoadFile` migrates old configs: `role:"guest"` → `RequireLogin=false`, `role:"user"|"admin"` → `RequireLogin=true`. `conf.ValidRole` is gone.

- **`RequireLogin` (bool)** — when true, the proxy refuses requests that came from cloudbox (i.e. carry `X-Forwarded-Prefix`) unless they also carry `X-Periscope-Role`. That header is stamped only after the caller cleared the per-(host, app) `matrix_elev` cookie via cloudbox's elevate flow. Loopback hits (admin UI subpath, local `/app/<name>/*`) bypass the gate entirely — the LAN/loopback boundary is what gates them.
- **`LANOnlyPaths` ([]string)** — segment-anchored prefix list. A request matching one of these is 404'd when `X-Forwarded-Prefix` is present, so kiosk-style endpoints (e.g. `/kiosk`) can be reachable on the LAN but invisible through the cloud surface. `matchSegmentPrefix` makes `/kiosk` match `/kiosk` and `/kiosk/foo` but not `/kiosks-of-truth`.
- **`IndexPath` (string)** — landing sub-path the cloudbox SPA prepends when constructing this tile's URL. Lets two `AppConfig` rows on the same upstream open at different paths ("Class" and "Class Admin" pointing at one app, each with its own tile + Connect button + cookie scope). Outpost itself doesn't rewrite anything; the field is published via `/apps` for the cloud's benefit.

### Admin UI REST surface (mounted under `/api/*` by `adminui`)

All paths take session-cookie auth (or bypass gate when unpaired + loopback). Each is a one-line wrapper into the matching `admincore.Server` method — see "Admin UI HTTP layer" above for the package layout.

- `GET /api/status`, `POST /api/login`, `POST /api/logout`
- `GET /api/config` (Token redacted; presence reported as `has_token`), `POST /api/config/register`, `POST /api/config/builtins`, `POST /api/config/networking`
- `GET|POST /api/apps`, `DELETE /api/apps/:name`, `POST /api/apps/:name/provisioning-token/rotate`, `GET /api/apps/suggestions`
- `GET|POST /api/outbound`, `DELETE /api/outbound/:path`, `POST /api/outbound/:path/connect`, `POST /api/outbound/:path/disconnect`, `GET /api/outbound/suggestions`
- `POST /api/cluster/kubeconfig`, `DELETE /api/cluster/kubeconfig`
- `GET /api/mcp/credentials`, `POST /api/mcp/token/rotate`
- `POST /api/restart`

Outbound endpoints are only registered when `core.Deps().Outbound != nil` (i.e. once paired with an `access_token`). MCP-credential endpoints are only registered when `main.go` threaded `MCPToken` / `RotateMCPToken` closures in (skipped on test paths).

### Portal exchange (`internal/agent/portal/`)

`portal.Exchange(ctx, ExchangeRequest)` is the single definition of the `POST <server>/api/register/exchange` round-trip. Called by both the CLI `register` command and the admin UI's `/api/config/register` handler; keeping it in one place prevents the two callers from drifting on payload or response shape.

### matrix tunnel (`internal/agent/tunnel.go`)

Embeds the underlying tunnel-client library (`github.com/fatedier/frp/client`, aliased as `tunnelclient` in the imports) directly — no config file path. Builds proxies via the in-memory `source.ConfigSource`. Important transport details:

- `Protocol` may be `tcp` (default), `websocket`, or `wss`. For `ws`/`wss` it sets `Transport.TLS.Enable=false` (edge already terminates TLS — double-wrap breaks the handshake) and `HeartbeatInterval=30` (Cloudflare reaps idle WS at ~100 s; the tunnel library's default heartbeat is `-1`/disabled, which would kill the control conn). Production via Cloudflare / DO App Platform uses `wss`.
- `LoginFailExit=false` so the agent survives cloud restarts and dials again with the tunnel library's built-in retry.

### Auth (`internal/agent/auth.go`, `internal/agent/hostauth/`)

Two strategies, selected by whether `AuthURL` is set:

- **OS path (`AuthURL == ""`)**: the submitted username MUST refer to the agent's own running OS user — compared via `hostauth.SameUser`, which accepts the exact local form or the bare account name case-insensitively (so `alice` matches Windows' SAM-compatible `MACHINE\alice`; authentication always runs against the canonical form, so the widened match never changes who gets verified); `hostauth.Authenticator` verifies the password via dscl (macOS) / PAM (Linux) / LogonUserW (Windows). The platform implementations live in `internal/agent/hostauth/hostauth_{darwin,linux,linux_pam,windows}.go` split by build tags. Role defaults to `admin` (whoever can prove the OS password owns the box). If `AdminUsers` is non-empty it acts as an allowlist over the cloud-trusted `X-Periscope-User` header; missing entries get downgraded to `user`.
- **AuthURL path**: agent POSTs `{user,password}` to the external endpoint and trusts the returned `{user,role}`. `AdminUsers` is ignored. `--title` is required at register time because no OS user exists to derive a portal subtitle from.

The cloud's `/matrix/h/:host/elevate` is what proxies to `/auth`. The agent never mints session tokens — only the cloud does, because only the cloud has the OAuth-identified caller.

### Shell

`internal/agent/shell.go` glues WebSocket to `internal/agent/shell.Session` (a PTY-wrapped runner from the forked `mvdan.cc/sh/v3`). Three goroutines per connection: PTY→WS, runner, and the foreground WS→PTY loop. The package itself is `internal/agent/shell/` with `runner.go` + `runner_errs.go` + `env.go`, build-tagged `pty_{unix,windows}.go`, and `vpty.go`.

On Windows there is no kernel PTY an *in-process* runner can sit behind (ConPTY attaches a child process; the qiangli/sh interpreter lives in this process), so `openPTY` hands out the **virtual PTY** from `vpty.go`: a pipe-backed master/slave pair that emulates just enough line discipline — ONLCR (`\n`→`\r\n`) on runner output, ICRNL (`\r`→`\n`) on keystrokes, tracked window geometry. The remote terminal (SSH client after pty-req, or xterm.js) is already in raw mode and does the rendering; echo and line editing are readline's own, switched on via the fork-only `interactive.Options.AssumeTTY` (raw-mode enter/exit become no-ops). Two load-bearing details: the input direction is a real `os.Pipe` fd handed to `interp.StdIO` directly — any non-`*os.File` stdin makes interp spawn a copier goroutine that steals keystrokes from readline — and `vpty.go` is deliberately untagged so unix test runs cover the Windows code path (`vpty_test.go`, incl. a full interactive session). Same in-process bash on every platform: the SSH/shell surfaces never fall back to cmd.exe/powershell. Accepted v1 gaps vs. a real PTY: no SIGWINCH (size re-polled per prompt), `tty` reports "not a tty", ^C is a byte not a signal.

`Session.Run` delegates the interactive read-eval loop to **`mvdan.cc/sh/v3/interactive`** (a fork-only package in the sibling at `../sh/interactive/`). That package wraps `ergochat/readline` around `interp.Runner` to give matrix-shell + `/ssh` users arrow-key history navigation, cursor movement, Backspace/Ctrl-W/Ctrl-U/Ctrl-K editing, Ctrl-R reverse search, and history-file persistence. Without it, upstream `parser.Interactive`'s cooked-mode pass-through would deliver `\x1b[A` literally to the lexer on every up-arrow. The non-obvious wiring is `interactive.bindTTY`: `ergochat/readline`'s default raw-mode handler hardcodes `syscall.Stdin` (fd 0), which is wrong for any embedder driving a PTY slave on some other fd. `bindTTY` inspects `Options.Stdin` and, when it's an `*os.File` on a TTY, installs custom `FuncMakeRaw` / `FuncExitRaw` / `FuncIsTerminal` / `FuncGetSize` keyed off that fd. The PTY slave is in raw mode while readline is reading a line, and back to whatever termios the next command sets when running — so curses programs (`vim`, `htop`, `less`) see a real `/dev/ttysNN`.

History persists at `$OUTPOST_SHELL_HISTORY` if set, else `<UserCacheDir>/outpost/shell_history` (created with mode 0700 on first call). Both the browser matrix-shell tab and an SSH session share the same file — they're the same Session.Run code path. In unit tests, the `ptyDrain` helper in `runner_test.go` doubles as a minimal terminal emulator: it answers readline's `\x1b[6n` DSR cursor-position query with `\x1b[1;1R` so the prompt actually renders. Production responders are xterm.js (browser) and the SSH client's local terminal emulator.

`shell.BuildEnv()` (in `env.go`) is what the runner gets via `interp.Env(...)`. It takes the outpost process's env (`os.Environ()`) and **prepends** to PATH: the outpost binary's own dir, `$HOME/bin`, `$HOME/.local/bin`, `/opt/homebrew/{bin,sbin}`, `/usr/local/{bin,sbin}` — dirs that exist and aren't already on PATH. This is load-bearing because launchd-spawned daemons get a very narrow PATH (`/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:/opt/homebrew/bin` on macOS LaunchDaemons), and the absence of `$HOME/bin` is what caused `which outpost` to return empty inside the matrix shell — turning `ls -la $(which outpost)` into `ls -la` (cwd listing). Same helper is used by `agent/ssh.go`'s `runExecCommand` (the SSH `exec` channel runner) — note that `exec`-channel commands bypass `interactive.Run` entirely; they're one-shot, not interactive.

`shell.CoreutilsExec` (in `coreutils.go`) is an `interp.ExecHandlers` middleware wired into all three runner constructors (interactive Session, `outpost shell` local runner, and `runExecCommand`): when a command name misses PATH but is implemented by the embedded pure-Go registry in `github.com/qiangli/coreutils/cmds/all` (~70 tools — `ls`, `cat`, `head`, `whoami`, `grep`, …), the embedded implementation runs in-process. A real binary on PATH always wins, so unix hosts see no behavior change; the fallback exists for Windows (and minimal containers), where agentic callers previously got 127 for every core tool. Operand/cwd resolution threads through `interp.HandlerCtx` (`tool.RunContext{Dir, Env, Stdio}`) — never the daemon process's globals.

### SSH

Two halves — a server inside the outpost agent and client-side CLI helpers:

- **Server (`internal/agent/ssh.go`)**: `GET /ssh` accepts the WebSocket, wraps it as a `net.Conn`, and hands it to an in-process `golang.org/x/crypto/ssh` server. Trust model mirrors `/shell` and `/desktop`: the submitted SSH username MUST refer to the agent's OS user (rejected pre-PAM otherwise; `hostauth.SameUser` accepts the bare account name against the qualified Windows form, same rule as `/auth`). When cloudbox stamps `X-Periscope-Role: user|admin` on the WSS upgrade — meaning the caller already cleared the `matrix_elev` gate via `/matrix/h/<host>/elevate` — the handler skips the SSH-protocol password challenge to avoid prompting twice for the same OS password. Without that header, the `PasswordCallback` delegates to the same `hostauth.Authenticator` used by `/auth` (PAM / dscl / LogonUserW). The channel dispatcher accepts three channel types: `session` (interactive shell + `exec`, the original use), `direct-tcpip` (stock `ssh -L` / `ssh -D` operator port-forwarding), and `direct-streamlocal@openssh.com` (the OpenSSH extension for unix-socket forwarding — the channel podman's `ssh://` URL transport uses). `direct-tcpip` is gated by `FileConfig.SSHAllowLocalForward` (default on) and by a loopback-only destination allowlist (`localhost` / `127.0.0.1` / `::1`); it adds no authority beyond what `ssh ... 'nc 127.0.0.1 PORT'` already provides via a session channel today, so the operator who can pass the OS-password gate already has equivalent reach. `direct-streamlocal@openssh.com` is gated by the same `SSHAllowLocalForward` toggle and by a unix-socket-path allowlist: the dynamic podman-socket candidates `DetectPodman()` probes (rootless `/run/user/<uid>/podman/podman.sock`, system `/run/podman/podman.sock`, macOS machine paths) plus the canonical docker sockets, plus operator-supplied paths in `FileConfig.SSHForwardSockets`. The allowlist is exact-match after `filepath.Clean` (so `/run/podman/../podman/podman.sock` resolves to the canonical form before lookup) — no globs, no symlink-following. This is what makes `podman --connection=<host>` work end-to-end through an ssh-scheme outbound; docker's `DOCKER_HOST=ssh://…` already rode the `exec` channel via `docker system dial-stdio` and didn't need streamlocal. See `docs/remote-podman.md` for the operator-facing setup. `tcpip-forward` / `cancel-tcpip-forward` global requests (`ssh -R`) are also honored — gated by `FileConfig.SSHAllowRemoteForward` (default on) and by the same loopback-only allowlist applied to the **bind** address (`allowTCPIPForwardBind`). A per-conn `forwardRegistry` tracks listeners; teardown is `defer fwds.closeAll()` on the SSH conn plus the `cancel-tcpip-forward` path. Each accepted connection on a `tcpip-forward` listener is pushed back as a `forwarded-tcpip` channel and byte-bridged with the same `io.Copy` pattern as `direct-tcpip`. The session-channel request loop also accepts `subsystem "sftp"` when `FileConfig.SFTPEnabled` (default on); the SFTP server is `github.com/pkg/sftp` wired straight onto the channel, scoped only by the OS user's filesystem permissions. Modern openssh `scp` (8.8+) uses SFTP under the hood, so this is what makes `scp host:foo .` work — without it scp falls back to `subsystem request failed`. Legacy `scp -O` rides the existing `exec` channel and worked before this. `pty-req` propagates the client's `TERM` into the runner env via `outshell.SessionOptions.Term` so `vim`/`htop`/`less` get a real terminal type instead of inheriting the daemon's (usually empty) TERM.
- **Host key (`internal/agent/hostkey.go`)**: persistent ed25519 keypair at `<UserConfigDir>/matrix/ssh_host_ed25519` (mode 0600). Kept out of `agent.json` *on purpose* so that re-pairing — which rewrites `agent.json` — does not regenerate the identity and trigger `REMOTE HOST IDENTIFICATION HAS CHANGED` for clients with cached `known_hosts`.
- **`outpost connect <host>` (`cmd/outpost/connect.go`)**: CLI mirror of the Periscope "Connect" button. Resolves the host's cloudbox view first (`/api/v1/ssh/hosts`: reported `os_user` + `shared` flag); for a host *shared with* this account it skips the password prompt entirely (cloudbox mints the sharee cookie from the HostShare row and never reads the password), otherwise prompts for the OS password and resolves the username (CLI flag → reported `os_user` → local `$USER`). POSTs to `<server>/matrix/h/<host>/elev/ssh` with the local outpost's bearer `access_token`, and caches the returned `matrix_elev` cookie at `~/.cache/outpost/sessions/<host>.cookie`. Reads the cookie off `resp.Cookies()` directly rather than via cookiejar — cloudbox scopes the cookie's Path to the data URL (`/matrix/h/<host>/ssh`), which is a sibling of the POST URL (`/matrix/h/<host>/elev/ssh`), so the jar would (correctly) drop it. Subsequent `ssh-proxy` runs ride on that cookie until idle (1 h) / absolute (8 h) expiry. `--stdin` reads the password from stdin for non-interactive agentic callers. `--keep-alive` holds the process open and pings `/matrix/h/<host>/elev/ssh/ping` every 30 min to slide the idle TTL (cloudbox refreshes Set-Cookie past the halfway mark; we capture the refreshed value via `resp.Cookies()` and atomically rewrite the cache file). The process exits non-zero on 401/403 so a supervisor wrapper can re-elevate with a fresh password; SIGTERM/SIGINT exits cleanly. Use for long-running agentic flows that would otherwise hit `EAUTHREQUIRED` mid-run.
- **`outpost ssh-proxy <host>` (`cmd/outpost/ssh.go`)**: meant as a `ProxyCommand` in `~/.ssh/config`. Opens a WebSocket to `<cloudbox>/matrix/h/<host>/ssh` with the persisted bearer token, attaches the cached `matrix_elev` cookie if present, and pipes stdin↔WS↔stdout. Local `ssh` does the SSH protocol on top.
- **`outpost ssh-config`**: emits `~/.ssh/config` stanzas for hosts visible to this account (uses the same persisted bearer token).
- **`outpost sshd` (`cmd/outpost/sshd.go`)**: standalone foreground LAN SSH server — the same in-process server (`agent.ServeLANSSH`, same persistent host key, shell + exec + SFTP + forwarding), bound to a plain TCP port (default `:2222`, all interfaces) with **no daemon, no pairing, and no cloudbox** required. Drop-in user-space sshd for machines without one (default macOS / Windows) and for bootstrap ("target machine has only the outpost binary; drive setup from a laptop over the LAN"). Auth is always the OS-password gate (no vouching paths exist on this listener). Advertises over mDNS by default (`--no-mdns` opts out) so sibling machines find it by name. Paired extras (peer direct-tcpip allowlist, cloudbox-tunneled `ssh -J` hops) light up opportunistically when an `agent.json` with an `access_token` is present. Distinct from the daemon-managed `ssh_listen_addr` listener, which is the always-on variant of the same thing.
- **Client-side no-cloudbox fallback (`dialOutpostHost` / `dialSSHTargetChain`)**: the `outpost ssh` / `scp` / `shasum` / `ssh sftp` client family never *requires* cloudbox. An explicit port (`outpost ssh user@host:2222`, `scp -P 2222`, `shasum -P 2222`) forces a plain TCP dial; so does a `.local` host name or an IP literal (`isLANAddressLiteral` — RFC 6762 names and IPs can never be cloudbox host names, so the cloudbox detour would only burn a round-trip + password prompt before falling back anyway); an unpaired machine automatically takes the LAN path (mDNS lan-ssh endpoint lookup, else `host:2222`); a paired machine falls back to the same LAN path when the whole cloudbox-assisted flow fails (internet down). LAN-direct auth is the remote OS password via `sshclient.Config.Auth` (TTY prompt, or `$OUTPOST_SSH_PASSWORD` for non-interactive callers), host key pinned TOFU into the same known_hosts. Saved `--direct` targets likewise no longer demand pairing.

### Outbound mounts (`internal/agent/outbound.go`)

`OutboundManager` registers local-mount-path → remote-outpost-app mappings. Lifecycle for one mount:

```
  Register      Connect(pw)           Disconnect / pinger-failure
cfg only ── elev cookie + pinger ── back to cfg-only
```

`Connect` calls cloudbox's elevate endpoint with `Bearer <access_token>` + `{user, password}`:
- http/tcp scheme → `POST /matrix/h/<host>/elev/app/<name>` (cookie Path scopes to `/matrix/h/<host>/app/<name>` — two mounts to the same remote host have isolated cookies)
- ssh scheme → `POST /matrix/h/<host>/elev/ssh` (cookie Path scopes to `/matrix/h/<host>/ssh`)

`/elev/` is a literal path segment in the cloudbox routing tree (not a suffix), introduced to avoid collision with gin's catch-all wildcard on `/matrix/h/:host/app/:name`. The legacy `/matrix/h/<host>/elevate` returns 410; its hint message names a suffix-style URL that **doesn't actually exist** — the real routes all sit under `/matrix/h/<host>/elev/...`.

Captures the `matrix_elev` cookie and starts a 4-minute pinger (`/matrix/h/<host>/elev/<app|ssh>/ping`) to slide the idle TTL. `Disconnect` (or a pinger failure indicating absolute expiry) drops the cookie; the operator must `Connect` again. Cookies are **never** persisted to disk — only `conf.OutboundConfig` is (stored in `FileConfig.Outbound`). Outbound paths share the local `NoRoute` namespace with custom apps — the admin handler refuses to register an outbound that would shadow a local app name.

The manager is only constructed when `fc.AccessToken` is present, so unpaired outposts don't expose the outbound endpoints.

**Three transports:** `OutboundConfig.Scheme` selects:
- `http` (default) — admin-UI subpath at `http://localhost:17777/<path>/...` proxied through cloudbox to the remote app's HTTP endpoint.
- `tcp` — a `127.0.0.1:<local_port>` listener that byte-bridges every accepted TCP conn to the remote outpost via WSS to `/matrix/h/<host>/app/<name>/`. Requires a matching `tcp`-scheme `AppConfig` on the remote outpost. Lets unmodified clients reach non-HTTP services hosted on the remote machine — `psql -h 127.0.0.1 -p <local_port>`, `mysql -h 127.0.0.1 -P <local_port>`, etc.
- `ssh` — same listener+WS-bridge shape as `tcp`, but the bridge targets the remote outpost's **built-in `/ssh` endpoint** (the in-process `golang.org/x/crypto/ssh` server) directly, dialing `wss://<cloudbox>/matrix/h/<host>/ssh`. No `AppConfig` on the remote required. The `Name` field is ignored. Elevate flow uses the per-builtin cloudbox endpoint `POST /matrix/h/<host>/elev/ssh` (with `elev` as a literal segment — *not* `/matrix/h/<host>/ssh/elevate`, even though the 410 handler that replaced the legacy host-wide `/matrix/h/<host>/elevate` hints at the suffix form; the real route uses `/elev/` to avoid colliding with gin's catch-all on `/matrix/h/:host/app/:name`). Pinger hits `/matrix/h/<host>/elev/ssh/ping`. Use case: `ssh -p <local_port> noviadmin@127.0.0.1` from a roaming dragon to reach a paired outpost's built-in SSH without needing a host-OS sshd port mapping.

TCP-mode wire flow (one accepted conn):

```
ssh / psql / …                    127.0.0.1:<local_port>            local outpost
                       ──────────►                       ──► tcpAcceptLoop
                                                            └─► websocket.Dial wss://<cloudbox>/matrix/h/<host>/app/<name>/
                                                                with Bearer + matrix_elev cookie
                                                                bytes flow both ways through NetConn
```

On the remote outpost, the same `/app/:name/*p` route inspects the registered app's scheme — for `tcp`, it accepts the WS upgrade and dials the configured `host:port` (e.g. `127.0.0.1:22`, `127.0.0.1:5432`) and byte-splices. See `serveTCPBridge` in `apps.go`.

Constraints / behavior:
- Connect binds the listener synchronously; an `EADDRINUSE` surfaces to the caller instead of getting buried in a goroutine.
- The admin handler refuses two `tcp` outbounds that want the same `local_port`.
- `Register` now tears down a surviving connection whenever the persisted cfg row changed (any field — scheme, local_port, name, host, user) so a stale listener can't keep the old port bound.
- HTTP requests to a `tcp`-scheme outbound at the loopback subpath return 400 — that's a category error (use the TCP port, not the admin UI).

**Cloudbox assumption:** the cloud route at `/matrix/h/<host>/app/<name>/...` must transparently forward WebSocket upgrades. `httputil.ReverseProxy` handles this natively, so a standard reverse-proxy setup in cloudbox needs no change for TCP mode to work end-to-end.

### Built-in proxy apps (`internal/agent/builtin_apps.go`)

Optional local-daemon proxies: `podman` and `ollama`. `DetectPodman()` probes the usual rootless/root socket paths; `DetectOllama()` probes `http://127.0.0.1:11434`. The returned `BuiltinTarget{Available bool, Socket/URL string}` lets the admin UI grey out toggles for daemons that aren't installed (still showing "tried <path>"). When enabled they register into the same `AppRegistry` as user-defined apps at boot, so flipping `PodmanEnabled` / `OllamaEnabled` from the admin UI triggers a self-restart.

### Ollama LLM pool (`internal/agent/ollama/`)

Beyond the per-host `/app/ollama/...` proxy described above, an outpost can join a **multi-host virtual LLM pool** that cloudbox fronts as a single OpenAI-compatible endpoint (`/v1/chat/completions`, `/v1/models`, etc.). The pool routes by model presence: a request for `llama3.2:1b` lands on whichever participating outpost actually has that model loaded. The outpost-side contribution is three small surfaces — registry push, capacity hint, and capabilities advertisement — all wired only when `fc.OllamaPoolOn()` (default-on whenever `OllamaEnabled` is on; explicit-off keeps the local Ollama private).

- **Registry push (`internal/agent/ollama/watcher.go`)** — every 30 s the watcher GETs the local daemon's `/api/tags`, diffs against the last published snapshot, and (on change OR every 5 min as a heartbeat) POSTs to `<cloudbox>/api/v1/llm/registry` with `Authorization: Bearer <access_token>`. Payload is `RegistryPushPayload{agent_name, version, heartbeat_at, models:[], capacity:{}}` defined in `types.go`. A 401 from cloudbox returns `ErrAuthRevoked` (pairing was pulled cloud-side); 5xx / network errors back off exponentially (5 s → 5 min cap) and retry. No access_token == no-op (the watcher logs once and blocks on ctx). Started under the same errgroup as the tunnel in `cmd/outpost/main.go`.
- **Capacity probe (`/app/ollama/_pool/capacity`)** — a per-app intercept mounted on the same `/app/ollama/*` tree the daemon proxy uses, so cloudbox reaches it through the existing per-(host, app) `matrix_elev` cookie authority. Returns `CapacityReport{max_parallel, in_flight}` as JSON. `max_parallel` is read once at boot from `$OLLAMA_NUM_PARALLEL` (default 4); `in_flight` is a live atomic counter incremented by `Counter.Wrap` for the duration of any request whose path matches a generation endpoint (`/api/chat`, `/api/generate`, `/api/embed*`, `/v1/chat/completions`, `/v1/completions`, `/v1/embeddings`). Non-generation paths (`/api/tags`, `/api/show`, `/api/ps`) pass through without bumping the counter. Same counter is shared with the watcher so push payload + capacity endpoint report identical numbers.
- **Capabilities advertisement** — when the ollama built-in registers, `apps.SetCapabilities("ollama", &AppCapabilities{Type:"llm"})` decorates the `/apps` entry so cloudbox's pool router can discover ollama-bearing outposts without a separate probe. `AppEntry.Capabilities` is `omitempty` — old cloudbox ignores the field.

**AppRegistry extension surface** (used by the ollama wiring, generic enough for future built-ins):
- `AddIntercept(name, prefix, http.Handler)` — binds a path-prefix handler that pre-empts the reverse proxy for matching requests under one app. Segment-anchored prefix match (so `/_pool` hits `/_pool/capacity` but not `/_poolish`). Used to mount metadata endpoints on the same auth-gated tree as the app itself.
- `SetProxyWrap(name, func(http.Handler) http.Handler)` — wraps the reverse proxy with middleware. Used for the in-flight counter. Passing nil clears.
- `SetCapabilities(name, *AppCapabilities)` — decorates an existing entry with a typed-app descriptor. No-op when name is unknown (so a typo at boot doesn't crash).

`OllamaPoolEnabled *bool` in `FileConfig` is the operator toggle (admin UI surfaces it under the Ollama row, dimmed when Ollama itself is off). Pool participation requires Ollama enabled + AccessToken present (paired). Push protocol contract lives in `internal/agent/ollama/types.go` — cloudbox decodes the same types from the request body.

### Cloudbox-pushed self-upgrade (`internal/agent/upgrade/`)

The daemon-side half of the "press button, fleet rolls" flow. Cloudbox initiates by POSTing an envelope to `<host>/admin/upgrade` through the matrix tunnel; the outpost downloads + atomically swaps its own binary + re-execs. The outpost decides what to do — cloudbox is a notifier, not a remote-control RPC.

- **`upgrade.Envelope`** — wire shape `{release_id, url, sha256, commit, min_from?}`. `Validate()` enforces required fields + https-only. `release_id` is opaque to outpost; used solely for dedup (`lastReleaseID` returns 200 Replay on duplicate POST — seeded from the ledger's newest `swap_done` entry at Worker construction so the guard survives the post-swap restart; a newer `rollback` entry clears the seed). The same-commit and `min_from` comparisons normalize both sides to 7-char shas (`shortCommit`), same as Probe — and `StateSnapshot.CurrentCommit` must be wired from `BuildInfo.ShortCommit()`, never `Short()` (which returns the version tag on release builds and can't match a sha; that mismatch plus the in-memory-only guard is what let the v0.7.0 fan-out re-apply on the canary and overwrite its `.previous` rollback copy).
- **`upgrade.Worker`** — singleton per daemon. `Apply(ctx, env)` runs the state machine (in-flight rejection 409 / replay 200 / disabled 403 / same-commit 304 / min-from 412 / accepted 202) under `mu`, then spawns the goroutine that does the actual work: StageFromURL (downloads + sha256-verifies) → Probe (`<candidate> version --json`, refused if commit mismatches envelope) → `retainPrevious` (hardlink-with-copy-fallback to `<binary>.previous` for rollback) → `os.Rename` (the atomic swap) → `restart` closure (wired to `core.ScheduleRestart`). Each phase emits one JSONL line to the ledger. The `State` closure re-reads FileConfig on every Apply so a just-flipped `auto_upgrade` takes effect immediately.
- **`upgrade.Ledger`** — JSONL appender at `<cacheDir>/outpost/upgrade.log`. One entry per phase (received, stage_failed, probe_failed, previous_unavailable, swap_done, rollback). `Tail(n)` reads the bounded newest-N for the `outpost upgrade history` CLI and the `outpost://upgrade-history` MCP resource. Append errors are logged-not-fatal — better to complete the upgrade than abort because we couldn't scribble a record.
- **`upgrade.MountRoute(rg, accessToken, worker)`** — gin handler factory. Constant-time bearer-token compare against `fc.AccessToken`. The factory pattern keeps `internal/agent` from importing `internal/agent/upgrade` (which would cycle through `agent.BuildInfo`): `agent.Deps.MountUpgradeRoute` is a closure threaded by `cmd/outpost/main.go` that calls into the upgrade package.
- **`upgrade.Rollback`** — single-shot swap of `<binary>.previous` back over the live binary. Probes the previous before swapping (refuses to swap a corrupted file). After rollback the `.previous` is gone; re-upgrade to climb forward again. Exposed via `outpost rollback` CLI + `outpost_rollback` MCP tool, both routed through the same `Worker.Rollback` so the dedup mutex blocks rollback while an upgrade is in flight.
- **`upgrade.Puller`** (`puller.go`) — the **pull trigger**, complementing the cloudbox-pushed path above. A goroutine (wired in `main.go` under the errgroup, guarded by `upgradeWorker != nil`) that, after an initial settle delay and then every ~10 min, GETs `<cloudbox>/api/v1/fleet/target?platform=<goos>_<goarch>` with `Bearer <access_token>`. On 200 it builds an **advisory** `Envelope` (`Force=false`) and hands it to `Worker.Apply`; 204 is a no-op. This is what lets a host that was asleep / offline when a release fanned out catch up on its next poll after the tunnel reconnects — **push reaches hosts online at fan-out time, pull reconciles the rest**. No new trust or policy: `Worker.Apply`'s `update_mode` gate (a `manual`/`never` host no-ops without `Force`) plus same-commit/replay checks make the poll a cheap no-op whenever the host is already current or has opted out of auto-upgrade. The cloudbox side returns the latest published release's artifact for the requested platform (the "target" is just the newest release).

Trust model: HTTPS-to-cloudbox + sha256-in-envelope + cloudbox-as-artifact-owner. An `ArtifactVerifier` hook is reserved for future signed-manifest validation but defaults to the no-op probe — the daemon will refuse a candidate that doesn't self-report the envelope's commit, which is the load-bearing check today.

CLI surfaces: `outpost upgrade` (local-driven, --local PATH | --from URL), `outpost upgrade history`, `outpost rollback`, `outpost builtins set --auto-upgrade=on/off`. MCP tools: `outpost_rollback`, `outpost_upgrade_history`, plus `auto_upgrade` slot on `outpost_set_builtins`. MCP resource: `outpost://upgrade-history`. The upgrade surface only mounts on paired hosts — the worker construction in main.go is guarded by `fc.AccessToken != ""`, and the MCP tools check `s.upgrader != nil` before registering.

### Cluster join — k3s-agent + virtual-kubelet (`internal/agent/{runtime,vkpodman,userkube}`, `cmd/outpost-{vk,cni}`)

The cluster story is the newest and most cross-cutting subsystem; it has three independent halves that share a `FileConfig.Cluster.{enabled, api_url, token, ca, node_name}` config block.

- **Half A — k3s-agent in a podman container (`internal/agent/runtime/`).** `runtime.Up(ctx, opts)` supervises a podman/docker container that hosts this outpost's kubelet + containerd. From the cluster's POV there's one `Node` per outpost; the container itself is invisible. The container's identity *is* this outpost's identity (`NodeToken`, `AgentName`). Idempotent — repeated `Up` with the same name reattaches. `ErrPodmanNotFound` surfaces a clear "install Docker Desktop / Rancher Desktop / podman to enable --cluster-mode=agent" message on macOS where this is the expected gating. `--cluster-mode=agent` is the default (see commit 20d3d14). Image is built once via `outpost cluster init` / `build-runtime` or pulled from a registry. See `docs/cluster-gpu.md` for the NVIDIA driver + container-toolkit prereqs needed on Linux GPU hosts.
- **Half B — virtual-kubelet provider (`internal/agent/vkpodman/`).** Alternative path: instead of running a real kubelet in a container, register as a *virtual node* whose pods land as plain podman containers on the host. Three layers — Layer 1: hand-rolled HTTP-over-unix client for the local libpod REST API (deliberately avoids `containers/podman/v5/pkg/bindings` because it pulls in `containers/storage` (cgo) and would break cross-compile); Layer 2: `translate.go` converts `corev1.Pod` → libpod `SpecGenerator`, stamping `outpost.io/managed=true` + namespace/name/uid labels for reconciliation and `podman ps` legibility; Layer 3: `provider.go` / `node.go` implement virtual-kubelet's `PodLifecycleHandler` and `NodeProvider`. `cmd/outpost-vk/` is a standalone PoC runner — kept separate from `cmd/outpost` so we can iterate against a real k3s server without touching the daemon's start path. `userkube` consumes vkpodman's kubeconfig fetcher for the operator-facing rendering.
- **Half C — CNI plugin (`internal/agent/runtime/image/cni/`).** Phase-3 minimal CNI 0.4.0 plugin (~300 LOC) that kubelet execs per pod ADD/DEL. Builds a veth pair into a per-node Linux bridge (`cbox0`); cross-node pod reachability comes from `tailscaled --advertise-routes` installing peer pod CIDRs as kernel routes via `tailscale0`. IPAM state at `/var/lib/cloudbox/cni/ipam/<container-id>.ip`. Deliberately not a full Calico/Cilium replacement — NetworkPolicy is out of scope for Phase 3 MVP. The plugin lives under the runtime image's embed tree so `outpost cluster build-runtime` compiles it inside the multi-stage `golang:1.25-alpine` builder; no standalone host-side cross-compile is involved (the committed `script/linux-outpost/outpost-cni` binary was retired in the same commit that introduced build-runtime).

Supporting packages:
- **`internal/agent/userkube/`** — materializes a kubectl-ready kubeconfig from cloudbox to disk. Three callers: daemon-at-startup (when `fc.Cluster.Enabled`), admin UI "Refresh" button, and `outpost cluster kubeconfig` CLI (now defaults to writing the file, was stdout). Path resolution + `LastStatus` for the UI live here so all three stay in sync. Default target: `$OUTPOST_KUBECONFIG_PATH` → `$HOME/.kube/outpost.yaml`.
- **`internal/agent/peerhosts/`** — TTL-cached snapshot of paired hostnames from cloudbox's `/api/v1/ssh/hosts`. Consumed by the SSH server's `direct-tcpip` allowlist so `ssh -J peerA peerB` works between paired hosts without widening trust to arbitrary destinations. Falls back to "last good snapshot" on cloudbox failure rather than denying — the inner SSH OS-password handshake is still the load-bearing gate.

### Smaller standalone packages

- **`internal/agent/sshagent.go`** — per-session `ssh -A` (auth-agent forwarding) support. Creates a 0700 tempdir + Unix socket, pushes accepted connections back via `auth-agent@openssh.com` channels, stamps `SSH_AUTH_SOCK` into the runner env. Gated by `FileConfig.SSHAllowAgentForward`. Tempdir is torn down on session-channel end.
- **`internal/agent/provision.go`** — `/_periscope/*` per-app provisioning relay. App-side caller authenticates with its per-app `ProvisioningToken`; outpost re-signs with `fc.AccessToken` and forwards to cloudbox at `/api/hosts/<host>/apps/<name>/grants`. 503s when unpaired (no `CloudboxBase` or `AccessToken`).
- **`internal/agent/sysinfo/`** + **`internal/agent/osversion/`** — host capability + OS-version probes (pure stdlib, no `gopsutil`). Shipped in the `/apps` poll loop under `system` so cloudbox can render host details and (future) make placement decisions for CPU-heavy pods. Per-platform behind build tags. Includes GPU probes (NVIDIA VRAM).
- **`internal/agent/ycode/`** — detection-only probe for a side-by-side `ycode serve` (the agentic engine outpost delegates to for inference, podman, OTel, etc.). Follows ycode's own TUI convention: a running instance publishes `$HOME/.agents/ycode/manifest.json` + `server.token`. Outpost reads + health-checks; admin UI shows "Running" / "Install ycode" / "installed (not running)" based on `State`. **Outpost never spawns or restarts ycode** — the operator owns ycode's lifecycle (and the CLI flags it was started with, which outpost has no way to reproduce). When ycode isn't running, the UI tells the operator to run `ycode serve` themselves.
- **`outpost git …`** — pure-Go git client, implemented by the sibling library `github.com/qiangli/coreutils/git` (umbrella mount `dhnt/coreutils`, sibling replace `../coreutils`; originally built here as `internal/agent/git` and extracted so ycode shares the same implementation). Verbs: clone, init, add, commit, status, log, diff, branch, checkout, push, pull, fetch, remote, show, merge, merge-base, rev-list, config, tag, reset, rm, ls-files, blame, grep, rev-parse. The load-bearing case is Windows-without-system-git: the library never shells out (even local-path remotes ride go-git's in-process server transport), `pull` is a hand-rolled fetch + fast-forward that preserves non-conflicting local changes, and diverged histories are an error by design. Conflict-resolution verbs (rebase, stash, cherry-pick, …) return clear pure-Go explanations with workaround hints — see `unimplementedGitVerbs` in `cmd/outpost/git.go`. HTTPS auth: `--username/--password`, else `$GITHUB_TOKEN` / `$GIT_TOKEN` oauth2-style. SSH auth: `--ssh-key [--ssh-key-pass]`. No MCP surface — classic `git` is CLI-only, so `outpost git` matches. Cobra wiring (incl. `outgit.CLIName = "outpost git"` for hint messages) in `cmd/outpost/git.go` + `cmd/outpost/git_verbs.go`; package internals documented in `coreutils/CLAUDE.md`.

## Conventions worth knowing

- "matrix-agent" and "outpost" refer to the same thing — older log messages say `matrix-agent:`. The portal-side namespace is "matrix"; the binary was renamed to "outpost" later.
- The portal contract for register lives at `POST <server>/api/register/exchange` and returns `{agent_name, server_addr, server_port, protocol, token, remote_port, access_token, client_only, auth_url}`. Any change to that response shape needs a coordinated portal change.
- `loopback-only` is load-bearing for the *main* (matrix-tunnel ingress) HTTP server: do not bind it to anything other than `127.0.0.1` — every code path assumes the matrix tunnel is the only ingress. The admin/agent-tool listener at `:17777` *can* be bound to LAN (operator override via `admin_addr`), and outpost warns + forces the auth gate on every request when it detects that.
- **Naming convention** (CLI / MCP / UI / file): the `agent.json` key is canonical. MCP arg names match it. CLI flags are kebab-case of the file key (`ssh_allow_local_forward` → `--ssh-allow-local-forward`). UI labels stay human-readable but each row shows the file key as a small monospace `.key-hint` badge. Historical short-form flags (e.g. `--ssh-local-fwd`) survive as deprecated aliases via `cobra.Flag.MarkDeprecated`. CLI subcommand verbs (`add`, `rm`, `list`) deliberately stay Unix-conventional and don't mirror MCP's `upsert/delete/list` — the audiences differ.
- New configuration features land in `admincore` first, with HTTP / MCP / CLI wrappers added in lockstep. A new operation isn't done until all four surfaces (file key, REST handler, MCP tool, CLI subcommand) reach the same admincore method. `docs/settings.md` is the place where the operator-visible side of that work gets documented.
- The pre-MCP `outpost outbound login/logout/list/add/connect/disconnect/rm` family uses session-cookie auth (its own `adminClient` in `cmd/outpost/outbound.go`); the newer subcommands (`apps`, `builtins`, `config`, `outbound suggest`, `cluster set/clear`, `status`, `restart`, `unpair`, `mcp rotate-token`) all go through `dialMCP()` and the bearer token. When adding a new CLI subcommand, default to the MCP path — it doesn't need a `login` dance and reuses the same `admincore` validation.
