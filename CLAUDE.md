# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

`outpost` is the home-host agent for the cloud portal at `ai.dhnt.io` (the **cloudbox** service). One binary runs on each machine the user wants to surface through the portal: it `register`s once with a one-time code, then `start` dials back through the matrix tunnel and serves local apps (HTTP reverse-proxy, PTY shell, VNC desktop, clipboard, /auth) so portal users can reach them at `https://ai.dhnt.io/h/<host>/app/<name>/`.

The local HTTP server binds loopback only — the cloud reaches it strictly through the matrix tunnel.

## Common commands

Requires Go 1.25+ (see `go.mod`). Note the `replace mvdan.cc/sh/v3 => github.com/qiangli/sh/v3 ...` in `go.mod` — the shell runner depends on a fork.

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
#   internal/agent/adminui/{adminui,e2e,suggestions,builtins,login_limiter}_test.go
go test ./...
go test ./internal/agent -run TestAuth
go test ./internal/agent/adminui -run TestE2E

# Run from source
go run ./cmd/outpost register --server https://ai.dhnt.io --code <code> --name <host>
go run ./cmd/outpost start
go run ./cmd/outpost stop
go run ./cmd/outpost connect <host>        # mirrors the web "Connect" button
go run ./cmd/outpost ssh-proxy <host>      # SSH ProxyCommand
go run ./cmd/outpost ssh-config            # emit ~/.ssh/config stanzas
```

`outpost start` no longer requires `register` first — on an unpaired host it brings up the admin UI and waits. `register` still exists for installer scripts and for users who want the whole pairing in one CLI invocation; `register --yes` (or answering "yes" to the prompt) re-execs the binary as a detached background process — see `cmd/outpost/main.go:execSelfStart` and `detach_unix.go` / `detach_windows.go`.

## Architecture

### Process layout

`cmd/outpost/main.go` wires the CLI surface: `start`, `register`, `stop`, plus the client-side helpers `connect`, `ssh-proxy`, `ssh-config` (defined in `cmd/outpost/connect.go` and `cmd/outpost/ssh.go`). `start` always launches the **admin UI** (loopback, default `127.0.0.1:17777`, override via `$OUTPOST_ADMIN_ADDR`). The admin UI is bound on its own listener — it is *never* advertised through the matrix tunnel, so it is local-machine only.

After the admin server is up `start` looks at the merged config:

- **Unconfigured** (`AgentName == ""`): print the admin URL, block on signal/restart. No tunnel, no main loopback server. The user opens the admin URL, pairs, and the admin handler triggers a self-restart.
- **Configured**: continue as before — bind a random loopback port for the main `gin.Engine` (`agent.RegisterRoutes`), build an embedded matrix-tunnel client (`agent.NewTunnel`) and dial `cfg.ServerAddr:ServerPort` with one TCP proxy pointing at the loopback port. All three (admin UI, main server, matrix tunnel) run in the same `errgroup`; cancelling the context shuts them all down.

`start` refuses to boot if another outpost owns the pidfile at `<UserCacheDir>/outpost/outpost.pid` (the matrix-tunnel `RemotePort` is fixed, so two instances would fight over the same slot). `stop` reads that pidfile, SIGTERMs, then SIGKILLs after 5 s.

### Self-restart for tunnel/identity changes

The matrix tunnel is immutable after `NewTunnel`, and the built-in routes (`/shell`, `/desktop`, `/clipboard`) are mounted conditionally at boot. So any admin-UI save that changes pairing, server URL, agent name, or a built-in toggle triggers a binary self-restart:

1. Handler writes the new config (`conf.SaveFile`).
2. Handler sends its JSON response, then 250 ms later calls the `restartFn` closure threaded down from `main.go`.
3. `restartFn` cancels the errgroup context (so all listeners drain).
4. After `g.Wait()` returns, the parent clears the pidfile (so the child can claim it), `execSelfStart`s a detached child, and exits.
5. The deferred `removePidFile` becomes a no-op via an `atomic.Bool` flag — without that, the parent would race-delete the child's freshly-written pidfile.

Custom-app add/edit/remove do *not* restart — `AppRegistry` is concurrency-safe, so the admin handler just mutates the live registry. Outbound mount add/remove/connect/disconnect also stays live (it only touches `OutboundManager`). Restart triggers are: pairing/identity changes, server URL, and any built-in toggle (`/shell`, `/desktop`, `/clipboard`, `/ssh`, plus the podman/ollama built-in proxy apps — those last two register into `AppRegistry` from boot, so flipping them on/off needs a restart).

### Config layering

`internal/agent/conf/`:

- `conf.Load()` reads env vars (`AGENT_*`, `MATRIX_*` — including `MATRIX_SERVER_ADDR`, `MATRIX_SERVER_PORT`, `MATRIX_TOKEN`, `MATRIX_PROTOCOL`, `MATRIX_REMOTE_PORT`, `MATRIX_APPS`, `MATRIX_ADMIN_USERS`, `MATRIX_AUTH_URL`).
- `conf.LoadFile(path)` reads the JSON written by `register` or the admin UI (default path: `<UserConfigDir>/matrix/agent.json` — XDG-aware).
- `start` layers them: env → file (only fills empty fields) → CLI flags override. The portal-returned `Protocol`/`Token`/`RemotePort`/`ServerAddr`/`ServerPort` come from the file.
- `FileConfig.Apps` (structured `[]AppConfig`) is the source of truth once it is present — even an empty slice wins over `MATRIX_APPS`. The legacy env path is still consulted when `fc.Apps == nil` (configs written before the admin UI shipped). Built-in toggles use `*bool` so a missing JSON key on an old config defaults to enabled; read via `fc.ShellOn() / DesktopOn() / ClipboardOn()`.

### Routes (`internal/agent/routes.go`)

All mounted at root:
- `GET /healthz`
- `GET /apps` — returns `{agent, apps:[{name,role}], builtins:{shell,desktop,clipboard,ssh}}`. Per-app `role` is the minimum clearance (`guest|user|admin`, default `user`) and the `builtins` map tells cloudbox which built-in routes this outpost actually mounted, so the portal can hide disabled tiles. Older outposts omit `builtins`; cloudbox treats that as legacy "all on".
- `POST /auth` — credential check (see Auth below)
- `GET /shell` — WebSocket PTY (binary frames = bytes, text frame `{"type":"size",...}` = resize)
- `GET /desktop` — WebSocket ↔ TCP VNC relay (`--vnc-addr`, default `127.0.0.1:5900`)
- `GET|POST /clipboard` — pbpaste/pbcopy bridge (works around RFB clipboard quirks)
- `GET /ssh` — WebSocket wrapped as a `net.Conn` and fed to an in-process `golang.org/x/crypto/ssh` server (see SSH section)
- `Any /app/:name/*p` — `httputil.ReverseProxy` to the URL registered under that name

`GET /apps`' `builtins` map covers `shell|desktop|clipboard|ssh`; `*bool` toggles in `FileConfig` (`ShellEnabled`, `DesktopEnabled`, `ClipboardEnabled`, `SSHEnabled`) default to enabled when absent for backwards-compat with old configs.

### Apps

`AppRegistry` (in `internal/agent/apps.go`) holds `name → *url.URL` plus per-app `httputil.ReverseProxy` instances and a per-app role (`guest|user|admin`, empty defaults to `user` — see `conf.ValidRole`). Concurrency-safe via `sync.RWMutex` — admin handlers `Register`/`Unregister` at runtime without touching the tunnel. `RegisterFromConfig(AppConfig)` is the helper that registers based on `AppConfig.Scheme`:

- `http`/`https` — TCP target built from `Host:Port` (Host defaults to `127.0.0.1`).
- `unix`/`npipe` — socket-backed. The registry stores a synthetic `http://socket` URL and a per-app `http.Transport` whose `DialContext` dials the local socket (`internal/agent/dialer{,_other,_windows}.go`). Lets an outpost front `docker.sock` / `podman.sock` / `\\.\pipe\docker_engine` without a TCP bind. HTTP/1.1 Upgrade and websockets still work because `httputil.ReverseProxy` hijacks the conn through this transport the same way it does for the default one.
- `tcp` — raw TCP target at `Host:Port` (e.g. `127.0.0.1:22` or `127.0.0.1:5432`). The `/app/:name/*p` handler doesn't run the reverse proxy for these; it accepts a WebSocket upgrade and byte-splices to the TCP target via `serveTCPBridge`. Paired with a `tcp`-scheme `OutboundConfig` on a peer outpost (see "Outbound mounts"). HTTP-mode and TCP-mode names are mutually exclusive — re-registering a name under a different scheme automatically clears the old mode.

Disabled entries are skipped so the admin UI can keep them around without proxying. Seeded by `buildAppRegistry` in `main.go` from `fc.Apps` when structured config is present, else from `MATRIX_APPS="name1=url1,name2=url2"`, falling back to `ycode → http://127.0.0.1:8765` when both are absent. Path rewrite uses `singleJoin` to strip `/app/<name>` cleanly. `Entries()` returns `[]AppEntry{Name, Role}` for `GET /apps`.

### Admin UI (`internal/agent/adminui/`)

Local-only web admin for pairing, built-in toggles, custom apps, and outbound mounts. Package layout: `server.go` (gin + listener + Serve(ctx)), `sessions.go` (in-memory cookie store, 1 h TTL, wiped on restart), `middleware.go` (gate engages once `fc.AgentName` is set), `handlers.go` (the API), `login_limiter.go` (per-IP token bucket on `POST /api/login`, default 5 burst / 12 s refill — see `login_limiter_test.go`), `ui.go` + `ui/index.html` (embedded vanilla-JS SPA via `//go:embed ui`).

API surface:
- `GET /api/status`, `POST /api/login`, `POST /api/logout`
- `GET /api/config` (Token redacted; presence reported as `has_token`), `POST /api/config/register`, `POST /api/config/builtins`
- `GET|POST /api/apps`, `DELETE /api/apps/:name`, `GET /api/apps/suggestions`
- `GET|POST /api/outbound`, `DELETE /api/outbound/:path`, `POST /api/outbound/:path/connect`, `POST /api/outbound/:path/disconnect`
- `POST /api/restart`

The `requireSession` middleware skips the gate while `AgentName == ""` (no paired identity to protect yet) — safe because the listener is loopback-only. Outbound endpoints are only registered when `deps.Outbound != nil` (i.e. once paired with an `access_token`).

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

`internal/agent/shell.go` glues WebSocket to `internal/agent/shell.Session` (a PTY-wrapped runner from the forked `mvdan.cc/sh/v3`). Three goroutines per connection: PTY→WS, runner, and the foreground WS→PTY loop. The package itself is `internal/agent/shell/` with `runner.go` + `runner_errs.go` and build-tagged `pty_{unix,windows}.go`.

### SSH

Two halves — a server inside the outpost agent and client-side CLI helpers:

- **Server (`internal/agent/ssh.go`)**: `GET /ssh` accepts the WebSocket, wraps it as a `net.Conn`, and hands it to an in-process `golang.org/x/crypto/ssh` server. Trust model mirrors `/shell` and `/desktop`: the submitted SSH username MUST equal the agent's OS user (rejected pre-PAM otherwise). When cloudbox stamps `X-Periscope-Role: user|admin` on the WSS upgrade — meaning the caller already cleared the `matrix_elev` gate via `/h/<host>/elevate` — the handler skips the SSH-protocol password challenge to avoid prompting twice for the same OS password. Without that header, the `PasswordCallback` delegates to the same `hostauth.Authenticator` used by `/auth` (PAM / dscl / LogonUserW).
- **Host key (`internal/agent/hostkey.go`)**: persistent ed25519 keypair at `<UserConfigDir>/matrix/ssh_host_ed25519` (mode 0600). Kept out of `agent.json` *on purpose* so that re-pairing — which rewrites `agent.json` — does not regenerate the identity and trigger `REMOTE HOST IDENTIFICATION HAS CHANGED` for clients with cached `known_hosts`.
- **`outpost connect <host>` (`cmd/outpost/connect.go`)**: CLI mirror of the Periscope "Connect" button. Prompts for the OS password of the user that the remote outpost runs as, POSTs to `<server>/h/<host>/elevate` with the local outpost's bearer `access_token`, and caches the returned `matrix_elev` cookie at `~/.cache/outpost/sessions/<host>.cookie`. Subsequent `ssh-proxy` runs ride on that cookie until idle (1 h) / absolute (8 h) expiry. `--stdin` reads the password from stdin for non-interactive agentic callers.
- **`outpost ssh-proxy <host>` (`cmd/outpost/ssh.go`)**: meant as a `ProxyCommand` in `~/.ssh/config`. Opens a WebSocket to `<cloudbox>/h/<host>/ssh` with the persisted bearer token, attaches the cached `matrix_elev` cookie if present, and pipes stdin↔WS↔stdout. Local `ssh` does the SSH protocol on top.
- **`outpost ssh-config`**: emits `~/.ssh/config` stanzas for hosts visible to this account (uses the same persisted bearer token).

### Outbound mounts (`internal/agent/outbound.go`)

`OutboundManager` registers local-mount-path → remote-outpost-app mappings. Lifecycle for one mount:

```
  Register      Connect(pw)           Disconnect / pinger-failure
cfg only ── elev cookie + pinger ── back to cfg-only
```

`Connect` calls cloudbox's `POST /h/<host>/elevate` with `Bearer <access_token>` + `{user, password}`, captures the `matrix_elev` cookie, and starts a 4-minute pinger to slide the idle TTL. `Disconnect` (or a pinger failure indicating absolute expiry) drops the cookie; the operator must `Connect` again. Cookies are **never** persisted to disk — only `conf.OutboundConfig` is (stored in `FileConfig.Outbound`). Outbound paths share the local `NoRoute` namespace with custom apps — the admin handler refuses to register an outbound that would shadow a local app name.

The manager is only constructed when `fc.AccessToken` is present, so unpaired outposts don't expose the outbound endpoints.

**Two transports:** `OutboundConfig.Scheme` selects either `http` (default — admin-UI subpath at `http://localhost:17777/<path>/...` proxied through cloudbox to the remote app's HTTP endpoint) or `tcp` (a `127.0.0.1:<local_port>` listener that byte-bridges every accepted TCP conn to the remote outpost via WSS). The TCP mode is what lets unmodified clients reach non-HTTP services as if they were local — `ssh -p <local_port> alice@127.0.0.1`, `psql -h 127.0.0.1 -p <local_port>`, `mysql -h 127.0.0.1 -P <local_port>`, etc.

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

## Conventions worth knowing

- "matrix-agent" and "outpost" refer to the same thing — older log messages say `matrix-agent:`. The portal-side namespace is "matrix"; the binary was renamed to "outpost" later.
- The portal contract for register lives at `POST <server>/api/register/exchange` and returns `{agent_name, server_addr, server_port, protocol, token, remote_port}`. Any change to that response shape needs a coordinated portal change.
- `loopback-only` is load-bearing: do not bind the local HTTP server to anything other than `127.0.0.1` — every code path assumes the matrix tunnel is the only ingress.
