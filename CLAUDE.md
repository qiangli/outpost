# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

`outpost` is the home-host agent for the cloud portal at `ai.dhnt.io` (the **cloudbox** service). One binary runs on each machine the user wants to surface through the portal: it `register`s once with a one-time code, then `start` dials back through the matrix tunnel and serves local apps (HTTP reverse-proxy, PTY shell, VNC desktop, clipboard, /auth) so portal users can reach them at `https://ai.dhnt.io/h/<host>/app/<name>/`.

The local HTTP server binds loopback only — the cloud reaches it strictly through the matrix tunnel.

## Common commands

Requires Go 1.25+ (see `go.mod`). Note the `replace mvdan.cc/sh/v3 => ../sh` in `go.mod` — the shell runner depends on a fork. The fork additionally implements `disown` / `kill` / `nohup` / `setsid` as builtins (upstream has them only as declarations or not at all), which is what lets `nohup ... &` survive a closed SSH session in the matrix shell. The fork also ships `mvdan.cc/sh/v3/interactive` — a reusable read-edit-execute loop wrapping `ergochat/readline` around `interp.Runner`, originally extracted from `cmd/bashy`; this is what gives the matrix shell + `/ssh` arrow-key history, cursor editing, and Ctrl-R reverse search that upstream `parser.Interactive` does not provide.

The sibling-path replace resolves in two contexts: inside the dhnt umbrella it points at the `dhnt/sh` submodule; standalone, run `make bootstrap` (or `./scripts/bootstrap-siblings.sh`) to clone it into `../sh` at the SHA pinned in `.sibling-pins`. CI runs the bootstrap automatically.

```bash
# Make targets (see `make help`)
make build      # → ./bin/outpost
make install    # go install ./cmd/outpost
make tidy       # go mod tidy + go fmt ./... + go vet ./...
make clean

# Tests (no `make test` target). Test files live under:
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
go run ./cmd/outpost cluster {kubeconfig,set,clear}
go run ./cmd/outpost mcp {endpoint,rotate-token}
go run ./cmd/outpost {restart,unpair}

# Client-side helpers (unchanged):
go run ./cmd/outpost connect <host>        # mirrors the web "Connect" button
go run ./cmd/outpost ssh-proxy <host>      # SSH ProxyCommand
go run ./cmd/outpost ssh-config            # emit ~/.ssh/config stanzas
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

- **OS path (`AuthURL == ""`)**: the submitted username MUST equal the agent's own running OS user; `hostauth.Authenticator` verifies the password via dscl (macOS) / PAM (Linux) / LogonUserW (Windows). The platform implementations live in `internal/agent/hostauth/hostauth_{darwin,linux,linux_pam,windows}.go` split by build tags. Role defaults to `admin` (whoever can prove the OS password owns the box). If `AdminUsers` is non-empty it acts as an allowlist over the cloud-trusted `X-Periscope-User` header; missing entries get downgraded to `user`.
- **AuthURL path**: agent POSTs `{user,password}` to the external endpoint and trusts the returned `{user,role}`. `AdminUsers` is ignored. `--title` is required at register time because no OS user exists to derive a portal subtitle from.

The cloud's `/h/:host/elevate` is what proxies to `/auth`. The agent never mints session tokens — only the cloud does, because only the cloud has the OAuth-identified caller.

### Shell

`internal/agent/shell.go` glues WebSocket to `internal/agent/shell.Session` (a PTY-wrapped runner from the forked `mvdan.cc/sh/v3`). Three goroutines per connection: PTY→WS, runner, and the foreground WS→PTY loop. The package itself is `internal/agent/shell/` with `runner.go` + `runner_errs.go` + `env.go` and build-tagged `pty_{unix,windows}.go`.

`Session.Run` delegates the interactive read-eval loop to **`mvdan.cc/sh/v3/interactive`** (a fork-only package in the sibling at `../sh/interactive/`). That package wraps `ergochat/readline` around `interp.Runner` to give matrix-shell + `/ssh` users arrow-key history navigation, cursor movement, Backspace/Ctrl-W/Ctrl-U/Ctrl-K editing, Ctrl-R reverse search, and history-file persistence. Without it, upstream `parser.Interactive`'s cooked-mode pass-through would deliver `\x1b[A` literally to the lexer on every up-arrow. The non-obvious wiring is `interactive.bindTTY`: `ergochat/readline`'s default raw-mode handler hardcodes `syscall.Stdin` (fd 0), which is wrong for any embedder driving a PTY slave on some other fd. `bindTTY` inspects `Options.Stdin` and, when it's an `*os.File` on a TTY, installs custom `FuncMakeRaw` / `FuncExitRaw` / `FuncIsTerminal` / `FuncGetSize` keyed off that fd. The PTY slave is in raw mode while readline is reading a line, and back to whatever termios the next command sets when running — so curses programs (`vim`, `htop`, `less`) see a real `/dev/ttysNN`.

History persists at `$OUTPOST_SHELL_HISTORY` if set, else `<UserCacheDir>/outpost/shell_history` (created with mode 0700 on first call). Both the browser matrix-shell tab and an SSH session share the same file — they're the same Session.Run code path. In unit tests, the `ptyDrain` helper in `runner_test.go` doubles as a minimal terminal emulator: it answers readline's `\x1b[6n` DSR cursor-position query with `\x1b[1;1R` so the prompt actually renders. Production responders are xterm.js (browser) and the SSH client's local terminal emulator.

`shell.BuildEnv()` (in `env.go`) is what the runner gets via `interp.Env(...)`. It takes the outpost process's env (`os.Environ()`) and **prepends** to PATH: the outpost binary's own dir, `$HOME/bin`, `$HOME/.local/bin`, `/opt/homebrew/{bin,sbin}`, `/usr/local/{bin,sbin}` — dirs that exist and aren't already on PATH. This is load-bearing because launchd-spawned daemons get a very narrow PATH (`/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:/opt/homebrew/bin` on macOS LaunchDaemons), and the absence of `$HOME/bin` is what caused `which outpost` to return empty inside the matrix shell — turning `ls -la $(which outpost)` into `ls -la` (cwd listing). Same helper is used by `agent/ssh.go`'s `runExecCommand` (the SSH `exec` channel runner) — note that `exec`-channel commands bypass `interactive.Run` entirely; they're one-shot, not interactive.

### SSH

Two halves — a server inside the outpost agent and client-side CLI helpers:

- **Server (`internal/agent/ssh.go`)**: `GET /ssh` accepts the WebSocket, wraps it as a `net.Conn`, and hands it to an in-process `golang.org/x/crypto/ssh` server. Trust model mirrors `/shell` and `/desktop`: the submitted SSH username MUST equal the agent's OS user (rejected pre-PAM otherwise). When cloudbox stamps `X-Periscope-Role: user|admin` on the WSS upgrade — meaning the caller already cleared the `matrix_elev` gate via `/h/<host>/elevate` — the handler skips the SSH-protocol password challenge to avoid prompting twice for the same OS password. Without that header, the `PasswordCallback` delegates to the same `hostauth.Authenticator` used by `/auth` (PAM / dscl / LogonUserW). The channel dispatcher accepts three channel types: `session` (interactive shell + `exec`, the original use), `direct-tcpip` (stock `ssh -L` / `ssh -D` operator port-forwarding), and `direct-streamlocal@openssh.com` (the OpenSSH extension for unix-socket forwarding — the channel podman's `ssh://` URL transport uses). `direct-tcpip` is gated by `FileConfig.SSHAllowLocalForward` (default on) and by a loopback-only destination allowlist (`localhost` / `127.0.0.1` / `::1`); it adds no authority beyond what `ssh ... 'nc 127.0.0.1 PORT'` already provides via a session channel today, so the operator who can pass the OS-password gate already has equivalent reach. `direct-streamlocal@openssh.com` is gated by the same `SSHAllowLocalForward` toggle and by a unix-socket-path allowlist: the dynamic podman-socket candidates `DetectPodman()` probes (rootless `/run/user/<uid>/podman/podman.sock`, system `/run/podman/podman.sock`, macOS machine paths) plus the canonical docker sockets, plus operator-supplied paths in `FileConfig.SSHForwardSockets`. The allowlist is exact-match after `filepath.Clean` (so `/run/podman/../podman/podman.sock` resolves to the canonical form before lookup) — no globs, no symlink-following. This is what makes `podman --connection=<host>` work end-to-end through an ssh-scheme outbound; docker's `DOCKER_HOST=ssh://…` already rode the `exec` channel via `docker system dial-stdio` and didn't need streamlocal. See `docs/remote-podman.md` for the operator-facing setup. `tcpip-forward` / `cancel-tcpip-forward` global requests (`ssh -R`) are also honored — gated by `FileConfig.SSHAllowRemoteForward` (default on) and by the same loopback-only allowlist applied to the **bind** address (`allowTCPIPForwardBind`). A per-conn `forwardRegistry` tracks listeners; teardown is `defer fwds.closeAll()` on the SSH conn plus the `cancel-tcpip-forward` path. Each accepted connection on a `tcpip-forward` listener is pushed back as a `forwarded-tcpip` channel and byte-bridged with the same `io.Copy` pattern as `direct-tcpip`. The session-channel request loop also accepts `subsystem "sftp"` when `FileConfig.SFTPEnabled` (default on); the SFTP server is `github.com/pkg/sftp` wired straight onto the channel, scoped only by the OS user's filesystem permissions. Modern openssh `scp` (8.8+) uses SFTP under the hood, so this is what makes `scp host:foo .` work — without it scp falls back to `subsystem request failed`. Legacy `scp -O` rides the existing `exec` channel and worked before this. `pty-req` propagates the client's `TERM` into the runner env via `outshell.SessionOptions.Term` so `vim`/`htop`/`less` get a real terminal type instead of inheriting the daemon's (usually empty) TERM.
- **Host key (`internal/agent/hostkey.go`)**: persistent ed25519 keypair at `<UserConfigDir>/matrix/ssh_host_ed25519` (mode 0600). Kept out of `agent.json` *on purpose* so that re-pairing — which rewrites `agent.json` — does not regenerate the identity and trigger `REMOTE HOST IDENTIFICATION HAS CHANGED` for clients with cached `known_hosts`.
- **`outpost connect <host>` (`cmd/outpost/connect.go`)**: CLI mirror of the Periscope "Connect" button. Prompts for the password first, then resolves the remote OS username (CLI flag → remote outpost's reported `os_user` → local `$USER`), POSTs to `<server>/h/<host>/elev/ssh` with the local outpost's bearer `access_token`, and caches the returned `matrix_elev` cookie at `~/.cache/outpost/sessions/<host>.cookie`. Reads the cookie off `resp.Cookies()` directly rather than via cookiejar — cloudbox scopes the cookie's Path to the data URL (`/h/<host>/ssh`), which is a sibling of the POST URL (`/h/<host>/elev/ssh`), so the jar would (correctly) drop it. Subsequent `ssh-proxy` runs ride on that cookie until idle (1 h) / absolute (8 h) expiry. `--stdin` reads the password from stdin for non-interactive agentic callers. `--keep-alive` holds the process open and pings `/h/<host>/elev/ssh/ping` every 30 min to slide the idle TTL (cloudbox refreshes Set-Cookie past the halfway mark; we capture the refreshed value via `resp.Cookies()` and atomically rewrite the cache file). The process exits non-zero on 401/403 so a supervisor wrapper can re-elevate with a fresh password; SIGTERM/SIGINT exits cleanly. Use for long-running agentic flows that would otherwise hit `EAUTHREQUIRED` mid-run.
- **`outpost ssh-proxy <host>` (`cmd/outpost/ssh.go`)**: meant as a `ProxyCommand` in `~/.ssh/config`. Opens a WebSocket to `<cloudbox>/h/<host>/ssh` with the persisted bearer token, attaches the cached `matrix_elev` cookie if present, and pipes stdin↔WS↔stdout. Local `ssh` does the SSH protocol on top.
- **`outpost ssh-config`**: emits `~/.ssh/config` stanzas for hosts visible to this account (uses the same persisted bearer token).

### Outbound mounts (`internal/agent/outbound.go`)

`OutboundManager` registers local-mount-path → remote-outpost-app mappings. Lifecycle for one mount:

```
  Register      Connect(pw)           Disconnect / pinger-failure
cfg only ── elev cookie + pinger ── back to cfg-only
```

`Connect` calls cloudbox's elevate endpoint with `Bearer <access_token>` + `{user, password}`:
- http/tcp scheme → `POST /h/<host>/elev/app/<name>` (cookie Path scopes to `/h/<host>/app/<name>` — two mounts to the same remote host have isolated cookies)
- ssh scheme → `POST /h/<host>/elev/ssh` (cookie Path scopes to `/h/<host>/ssh`)

`/elev/` is a literal path segment in the cloudbox routing tree (not a suffix), introduced to avoid collision with gin's catch-all wildcard on `/h/:host/app/:name`. The legacy `/h/<host>/elevate` returns 410; its hint message names a suffix-style URL that **doesn't actually exist** — the real routes all sit under `/h/<host>/elev/...`.

Captures the `matrix_elev` cookie and starts a 4-minute pinger (`/h/<host>/elev/<app|ssh>/ping`) to slide the idle TTL. `Disconnect` (or a pinger failure indicating absolute expiry) drops the cookie; the operator must `Connect` again. Cookies are **never** persisted to disk — only `conf.OutboundConfig` is (stored in `FileConfig.Outbound`). Outbound paths share the local `NoRoute` namespace with custom apps — the admin handler refuses to register an outbound that would shadow a local app name.

The manager is only constructed when `fc.AccessToken` is present, so unpaired outposts don't expose the outbound endpoints.

**Three transports:** `OutboundConfig.Scheme` selects:
- `http` (default) — admin-UI subpath at `http://localhost:17777/<path>/...` proxied through cloudbox to the remote app's HTTP endpoint.
- `tcp` — a `127.0.0.1:<local_port>` listener that byte-bridges every accepted TCP conn to the remote outpost via WSS to `/h/<host>/app/<name>/`. Requires a matching `tcp`-scheme `AppConfig` on the remote outpost. Lets unmodified clients reach non-HTTP services hosted on the remote machine — `psql -h 127.0.0.1 -p <local_port>`, `mysql -h 127.0.0.1 -P <local_port>`, etc.
- `ssh` — same listener+WS-bridge shape as `tcp`, but the bridge targets the remote outpost's **built-in `/ssh` endpoint** (the in-process `golang.org/x/crypto/ssh` server) directly, dialing `wss://<cloudbox>/h/<host>/ssh`. No `AppConfig` on the remote required. The `Name` field is ignored. Elevate flow uses the per-builtin cloudbox endpoint `POST /h/<host>/elev/ssh` (with `elev` as a literal segment — *not* `/h/<host>/ssh/elevate`, even though the 410 handler that replaced the legacy host-wide `/h/<host>/elevate` hints at the suffix form; the real route uses `/elev/` to avoid colliding with gin's catch-all on `/h/:host/app/:name`). Pinger hits `/h/<host>/elev/ssh/ping`. Use case: `ssh -p <local_port> noviadmin@127.0.0.1` from a roaming dragon to reach a paired outpost's built-in SSH without needing a host-OS sshd port mapping.

TCP-mode wire flow (one accepted conn):

```
ssh / psql / …                    127.0.0.1:<local_port>            local outpost
                       ──────────►                       ──► tcpAcceptLoop
                                                            └─► websocket.Dial wss://<cloudbox>/h/<host>/app/<name>/
                                                                with Bearer + matrix_elev cookie
                                                                bytes flow both ways through NetConn
```

On the remote outpost, the same `/app/:name/*p` route inspects the registered app's scheme — for `tcp`, it accepts the WS upgrade and dials the configured `host:port` (e.g. `127.0.0.1:22`, `127.0.0.1:5432`) and byte-splices. See `serveTCPBridge` in `apps.go`.

Constraints / behavior:
- Connect binds the listener synchronously; an `EADDRINUSE` surfaces to the caller instead of getting buried in a goroutine.
- The admin handler refuses two `tcp` outbounds that want the same `local_port`.
- `Register` now tears down a surviving connection whenever the persisted cfg row changed (any field — scheme, local_port, name, host, user) so a stale listener can't keep the old port bound.
- HTTP requests to a `tcp`-scheme outbound at the loopback subpath return 400 — that's a category error (use the TCP port, not the admin UI).

**Cloudbox assumption:** the cloud route at `/h/<host>/app/<name>/...` must transparently forward WebSocket upgrades. `httputil.ReverseProxy` handles this natively, so a standard reverse-proxy setup in cloudbox needs no change for TCP mode to work end-to-end.

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

- **`upgrade.Envelope`** — wire shape `{release_id, url, sha256, commit, min_from?}`. `Validate()` enforces required fields + https-only. `release_id` is opaque to outpost; used solely for dedup (in-memory `lastReleaseID` returns 200 Replay on duplicate POST during the restart window).
- **`upgrade.Worker`** — singleton per daemon. `Apply(ctx, env)` runs the state machine (in-flight rejection 409 / replay 200 / disabled 403 / same-commit 304 / min-from 412 / accepted 202) under `mu`, then spawns the goroutine that does the actual work: StageFromURL (downloads + sha256-verifies) → Probe (`<candidate> version --json`, refused if commit mismatches envelope) → `retainPrevious` (hardlink-with-copy-fallback to `<binary>.previous` for rollback) → `os.Rename` (the atomic swap) → `restart` closure (wired to `core.ScheduleRestart`). Each phase emits one JSONL line to the ledger. The `State` closure re-reads FileConfig on every Apply so a just-flipped `auto_upgrade` takes effect immediately.
- **`upgrade.Ledger`** — JSONL appender at `<cacheDir>/outpost/upgrade.log`. One entry per phase (received, stage_failed, probe_failed, previous_unavailable, swap_done, rollback). `Tail(n)` reads the bounded newest-N for the `outpost upgrade history` CLI and the `outpost://upgrade-history` MCP resource. Append errors are logged-not-fatal — better to complete the upgrade than abort because we couldn't scribble a record.
- **`upgrade.MountRoute(rg, accessToken, worker)`** — gin handler factory. Constant-time bearer-token compare against `fc.AccessToken`. The factory pattern keeps `internal/agent` from importing `internal/agent/upgrade` (which would cycle through `agent.BuildInfo`): `agent.Deps.MountUpgradeRoute` is a closure threaded by `cmd/outpost/main.go` that calls into the upgrade package.
- **`upgrade.Rollback`** — single-shot swap of `<binary>.previous` back over the live binary. Probes the previous before swapping (refuses to swap a corrupted file). After rollback the `.previous` is gone; re-upgrade to climb forward again. Exposed via `outpost rollback` CLI + `outpost_rollback` MCP tool, both routed through the same `Worker.Rollback` so the dedup mutex blocks rollback while an upgrade is in flight.

Trust model: HTTPS-to-cloudbox + sha256-in-envelope + cloudbox-as-artifact-owner. An `ArtifactVerifier` hook is reserved for future signed-manifest validation but defaults to the no-op probe — the daemon will refuse a candidate that doesn't self-report the envelope's commit, which is the load-bearing check today.

CLI surfaces: `outpost upgrade` (local-driven, --local PATH | --from URL), `outpost upgrade history`, `outpost rollback`, `outpost builtins set --auto-upgrade=on/off`. MCP tools: `outpost_rollback`, `outpost_upgrade_history`, plus `auto_upgrade` slot on `outpost_set_builtins`. MCP resource: `outpost://upgrade-history`. The upgrade surface only mounts on paired hosts — the worker construction in main.go is guarded by `fc.AccessToken != ""`, and the MCP tools check `s.upgrader != nil` before registering.

## Conventions worth knowing

- "matrix-agent" and "outpost" refer to the same thing — older log messages say `matrix-agent:`. The portal-side namespace is "matrix"; the binary was renamed to "outpost" later.
- The portal contract for register lives at `POST <server>/api/register/exchange` and returns `{agent_name, server_addr, server_port, protocol, token, remote_port, access_token, client_only, auth_url}`. Any change to that response shape needs a coordinated portal change.
- `loopback-only` is load-bearing for the *main* (matrix-tunnel ingress) HTTP server: do not bind it to anything other than `127.0.0.1` — every code path assumes the matrix tunnel is the only ingress. The admin/agent-tool listener at `:17777` *can* be bound to LAN (operator override via `admin_addr`), and outpost warns + forces the auth gate on every request when it detects that.
- **Naming convention** (CLI / MCP / UI / file): the `agent.json` key is canonical. MCP arg names match it. CLI flags are kebab-case of the file key (`ssh_allow_local_forward` → `--ssh-allow-local-forward`). UI labels stay human-readable but each row shows the file key as a small monospace `.key-hint` badge. Historical short-form flags (e.g. `--ssh-local-fwd`) survive as deprecated aliases via `cobra.Flag.MarkDeprecated`. CLI subcommand verbs (`add`, `rm`, `list`) deliberately stay Unix-conventional and don't mirror MCP's `upsert/delete/list` — the audiences differ.
- New configuration features land in `admincore` first, with HTTP / MCP / CLI wrappers added in lockstep. A new operation isn't done until all four surfaces (file key, REST handler, MCP tool, CLI subcommand) reach the same admincore method. `docs/settings.md` is the place where the operator-visible side of that work gets documented.
- The pre-MCP `outpost outbound login/logout/list/add/connect/disconnect/rm` family uses session-cookie auth (its own `adminClient` in `cmd/outpost/outbound.go`); the newer subcommands (`apps`, `builtins`, `config`, `outbound suggest`, `cluster set/clear`, `status`, `restart`, `unpair`, `mcp rotate-token`) all go through `dialMCP()` and the bearer token. When adding a new CLI subcommand, default to the MCP path — it doesn't need a `login` dance and reuses the same `admincore` validation.
