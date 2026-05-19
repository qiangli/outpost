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

# Tests (only auth_test.go and clipboard_test.go exist today; no `make test` target)
go test ./...
go test ./internal/agent -run TestAuth

# Run from source
go run ./cmd/outpost register --server https://ai.dhnt.io --code <code> --name <host>
go run ./cmd/outpost start
go run ./cmd/outpost stop
```

`outpost start` no longer requires `register` first — on an unpaired host it brings up the admin UI and waits. `register` still exists for installer scripts and for users who want the whole pairing in one CLI invocation; `register --yes` (or answering "yes" to the prompt) re-execs the binary as a detached background process — see `cmd/outpost/main.go:execSelfStart` and `detach_unix.go` / `detach_windows.go`.

## Architecture

### Process layout

`cmd/outpost/main.go` is the entire CLI surface: `start`, `register`, `stop` (subcommands). `start` always launches the **admin UI** (loopback, default `127.0.0.1:17777`, override via `$OUTPOST_ADMIN_ADDR`). The admin UI is bound on its own listener — it is *never* advertised through the matrix tunnel, so it is local-machine only.

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

Custom-app add/edit/remove do *not* restart — `AppRegistry` is concurrency-safe, so the admin handler just mutates the live registry.

### Config layering

`internal/agent/conf/`:

- `conf.Load()` reads env vars (`AGENT_*`, `MATRIX_*` — including `MATRIX_SERVER_ADDR`, `MATRIX_SERVER_PORT`, `MATRIX_TOKEN`, `MATRIX_PROTOCOL`, `MATRIX_REMOTE_PORT`, `MATRIX_APPS`, `MATRIX_ADMIN_USERS`, `MATRIX_AUTH_URL`).
- `conf.LoadFile(path)` reads the JSON written by `register` or the admin UI (default path: `<UserConfigDir>/matrix/agent.json` — XDG-aware).
- `start` layers them: env → file (only fills empty fields) → CLI flags override. The portal-returned `Protocol`/`Token`/`RemotePort`/`ServerAddr`/`ServerPort` come from the file.
- `FileConfig.Apps` (structured `[]AppConfig`) is the source of truth once it is present — even an empty slice wins over `MATRIX_APPS`. The legacy env path is still consulted when `fc.Apps == nil` (configs written before the admin UI shipped). Built-in toggles use `*bool` so a missing JSON key on an old config defaults to enabled; read via `fc.ShellOn() / DesktopOn() / ClipboardOn()`.

### Routes (`internal/agent/routes.go`)

All mounted at root:
- `GET /healthz`, `GET /apps`
- `POST /auth` — credential check (see Auth below)
- `GET /shell` — WebSocket PTY (binary frames = bytes, text frame `{"type":"size",...}` = resize)
- `GET /desktop` — WebSocket ↔ TCP VNC relay (`--vnc-addr`, default `127.0.0.1:5900`)
- `GET|POST /clipboard` — pbpaste/pbcopy bridge (works around RFB clipboard quirks)
- `Any /app/:name/*p` — `httputil.ReverseProxy` to the URL registered under that name

### Apps

`AppRegistry` (in `internal/agent/apps.go`) holds `name → *url.URL` plus per-app `httputil.ReverseProxy` instances. Concurrency-safe via `sync.RWMutex` — admin handlers `Register`/`Unregister` at runtime without touching the tunnel. `RegisterFromConfig(AppConfig)` is the helper that builds `scheme://host:port` and registers, skipping disabled entries. Seeded by `buildAppRegistry` in `main.go` from `fc.Apps` when structured config is present, else from `MATRIX_APPS="name1=url1,name2=url2"`, falling back to `ycode → http://127.0.0.1:8765` when both are absent. Path rewrite uses `singleJoin` to strip `/app/<name>` cleanly.

### Admin UI (`internal/agent/adminui/`)

Local-only web admin for pairing, built-in toggles, and custom apps. New package with `server.go` (gin + listener + Serve(ctx)), `sessions.go` (in-memory cookie store, 1 h TTL, wiped on restart), `middleware.go` (gate engages once `fc.AgentName` is set), `handlers.go` (the API), `ui.go` + `ui/index.html` (embedded vanilla-JS SPA via `//go:embed ui`). API: `GET /api/status`, `POST /api/login`, `POST /api/logout`, `GET /api/config` (Token redacted; presence reported as `has_token`), `POST /api/config/register`, `POST /api/config/builtins`, `GET|POST /api/apps`, `DELETE /api/apps/:name`, `POST /api/restart`. The `requireSession` middleware skips the gate while `AgentName == ""` (no paired identity to protect yet) — safe because the listener is loopback-only.

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

## Conventions worth knowing

- "matrix-agent" and "outpost" refer to the same thing — older log messages say `matrix-agent:`. The portal-side namespace is "matrix"; the binary was renamed to "outpost" later.
- The portal contract for register lives at `POST <server>/api/register/exchange` and returns `{agent_name, server_addr, server_port, protocol, token, remote_port}`. Any change to that response shape needs a coordinated portal change.
- `loopback-only` is load-bearing: do not bind the local HTTP server to anything other than `127.0.0.1` — every code path assumes the matrix tunnel is the only ingress.
