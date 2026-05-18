# outpost

The home-host agent for [cloudbox](https://github.com/qiangli/cloudbox).

One outpost binary runs on each machine you want to surface through the
portal. It registers with a cloudbox using a one-time code, dials back
over a frp tunnel, and serves the local apps (HTTP, shell, desktop,
clipboard) so that authenticated portal users can reach them through
`https://your.cloudbox/h/<host>/app/<name>/`.

## Install

```bash
go install github.com/qiangli/outpost/cmd/outpost@latest
```

## Pair with cloudbox

1. Sign in to your cloudbox `/admin/`, open **Hosts**, click **Generate
   invite code**.
2. On the home machine:

   ```bash
   outpost register \
     --server https://your.cloudbox \
     --code   <one-time-code> \
     --name   laptop

   outpost start
   ```

`register` exchanges the code for the agent's persistent config (saved
to `~/.config/matrix/agent.json`) including the FRP transport — `wss`
in prod (cloudbox terminates TLS at the edge), `websocket` for local
dev, or `tcp` if you're self-hosting cloudbox on a Droplet with the raw
FRP port exposed.

`start` dials the tunnel and starts the local HTTP server. By default
ycode at `http://127.0.0.1:8765` is registered; declare more apps with:

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

All of these are reached only through cloudbox via the FRP tunnel —
outpost binds its HTTP server to loopback (`127.0.0.1:<random>`).

## Build

```bash
go build ./cmd/outpost
```

Requires Go 1.25+.
