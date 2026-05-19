# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

`outpost` is the home-host agent for the cloud portal at `ai.dhnt.io`. One binary runs on each machine the user wants to surface through the portal: it `register`s once with a one-time code, then `start` dials back through an FRP tunnel and serves local apps (HTTP reverse-proxy, PTY shell, VNC desktop, clipboard, /auth) so portal users can reach them at `https://ai.dhnt.io/h/<host>/app/<name>/`.

The local HTTP server binds loopback only — the cloud reaches it strictly through the FRP tunnel.

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

`register` followed by `start` is the normal pairing flow; `register` without `--code` is interactive. `register --yes` (or answering "yes" to the prompt) re-execs the binary as a detached background process — see `cmd/outpost/main.go:execSelfStart` and `detach_unix.go` / `detach_windows.go`.

## Architecture

### Process layout

`cmd/outpost/main.go` is the entire CLI surface: `start`, `register`, `stop` (subcommands). `start` does three things in an `errgroup`:

1. Bind a random loopback port and serve a `gin.Engine` with `agent.RegisterRoutes`.
2. Build an embedded FRP client (`agent.NewTunnel`) and `Run` it against `cfg.ServerAddr:ServerPort` with one TCP proxy pointing at the loopback port.
3. Wait on `SIGINT`/`SIGTERM`, then shut both down.

`start` refuses to boot if another outpost owns the pidfile at `<UserCacheDir>/outpost/outpost.pid` (the FRP `RemotePort` is fixed, so two instances would fight over the same slot). `stop` reads that pidfile, SIGTERMs, then SIGKILLs after 5 s.

### Config layering

`internal/agent/conf/`:

- `conf.Load()` reads env vars (`AGENT_*`, `FRP_*`, `MATRIX_APPS`, `MATRIX_ADMIN_USERS`, `MATRIX_AUTH_URL`).
- `conf.LoadFile(path)` reads the JSON written by `register` (default path: `<UserConfigDir>/matrix/agent.json` — XDG-aware).
- `start` layers them: env → file (only fills empty fields) → CLI flags override. The portal-returned `Protocol`/`Token`/`RemotePort`/`ServerAddr`/`ServerPort` come from the file.

### Routes (`internal/agent/routes.go`)

All mounted at root:
- `GET /healthz`, `GET /apps`
- `POST /auth` — credential check (see Auth below)
- `GET /shell` — WebSocket PTY (binary frames = bytes, text frame `{"type":"size",...}` = resize)
- `GET /desktop` — WebSocket ↔ TCP VNC relay (`--vnc-addr`, default `127.0.0.1:5900`)
- `GET|POST /clipboard` — pbpaste/pbcopy bridge (works around RFB clipboard quirks)
- `Any /app/:name/*p` — `httputil.ReverseProxy` to the URL registered under that name

### Apps

`AppRegistry` (in `internal/agent/apps.go`) holds `name → *url.URL` plus per-app `httputil.ReverseProxy` instances. Seeded by `buildAppRegistry` in `main.go` from `MATRIX_APPS="name1=url1,name2=url2"`; if unset, registers `ycode → http://127.0.0.1:8765` by default. Path rewrite uses `singleJoin` to strip `/app/<name>` cleanly.

### FRP tunnel (`internal/agent/tunnel.go`)

Embeds `github.com/fatedier/frp/client` directly — no config file path. Builds proxies via the in-memory `source.ConfigSource`. Important transport details:

- `Protocol` may be `tcp` (default), `websocket`, or `wss`. For `ws`/`wss` it sets `Transport.TLS.Enable=false` (edge already terminates TLS — double-wrap breaks the handshake) and `HeartbeatInterval=30` (Cloudflare reaps idle WS at ~100 s; FRP's default heartbeat is `-1`/disabled, which would kill the control conn). Production via Cloudflare / DO App Platform uses `wss`.
- `LoginFailExit=false` so the agent survives cloud restarts and dials again with FRP's built-in retry.

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
- `loopback-only` is load-bearing: do not bind the local HTTP server to anything other than `127.0.0.1` — every code path assumes the cloud's FRP proxy is the only ingress.
