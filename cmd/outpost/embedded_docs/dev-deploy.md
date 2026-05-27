# Direct-local dev → outpost deploy

Lightweight `make build && make test && make deploy` for your own apps,
with outpost as the runtime. No GitHub Actions, no webhooks, no
cloudbox round-trip. Suits two starting scenarios:

1. **Same machine** — dev box *is* the outpost host.
2. **LAN target** — dev box is on the same network as the outpost
   (e.g. it can reach `host.local`).

Container runtime in this guide is **podman**. Plain processes and k8s
follow the same shape with a swapped deploy step.

## Prerequisites

- Outpost paired and running on the target machine.
- Podman built-in enabled: `outpost builtins set --podman=on`.
- On the dev box, `podman` CLI (any recent version that has
  `system connection add`).
- For LAN targets only: outpost's admin listener bound to LAN
  (see the LAN section below).

## App-name convention

Each environment is a **separate AppConfig** in the registry:

| Branch / channel | App name in outpost |
|---|---|
| dev / feature work | `<repo>-dev` |
| staging / pre-prod | `<repo>-stage` |
| prod / main | `<repo>` |

You register each one once via `outpost apps add`; the deploy loop
only changes the container behind the row, never the row itself.

## Same-machine recipe

Drop this into a `Makefile` at the root of your app repo:

```make
APP    := myapp
TAG    := dev
PORT   := 18080

build:
	podman build -t localhost/$(APP):$(TAG) .

test:
	go test ./...

deploy: build test
	-podman rm -f $(APP)-dev
	podman run -d --name $(APP)-dev --restart unless-stopped \
		-p $(PORT):8080 localhost/$(APP):$(TAG)
	outpost apps add --name $(APP)-dev --url http://127.0.0.1:$(PORT)
```

First-time only: the final `outpost apps add` line *registers* the
app. Re-running `make deploy` upserts — the row is updated in place,
the proxy mount stays live, so cloudbox's tile keeps working without
flapping.

Verify:

```sh
podman ps                                       # myapp-dev should be running
curl -i http://127.0.0.1:18080/                 # the new build
outpost apps list                               # shows myapp-dev, enabled=true
```

## LAN-target recipe

Two extra setup steps before the Makefile.

### 1. On the outpost machine — bind the admin listener to the LAN

```sh
outpost config set --admin-addr 0.0.0.0:17777   # or pin a specific IP
outpost mcp endpoint                            # prints the bearer token
```

**Security note.** The MCP bearer is root-equivalent for outpost —
anyone on the LAN who has it can pair/unpair, edit apps, restart, etc.
Bind only to a LAN segment you trust, never to a public Wi-Fi or a
guest network. Rotate immediately if the bearer leaks
(`outpost mcp rotate-token --yes`).

The admin listener already enforces session-cookie auth on the SPA
side; the MCP path uses the bearer. There is no anonymous access.

### 2. On the dev machine — cache the credentials

```sh
outpost remote login outpost.local
#   Admin endpoint [outpost.local:17777]: <enter>
#   MCP bearer token: <paste from step 1>
```

That writes `~/.config/outpost/remotes/outpost.local.json` (mode 0600).
From now on, any outpost CLI subcommand can target it with `--remote
outpost.local` (or `OUTPOST_REMOTE=outpost.local`).

### 3. Optional — set up the podman remote connection

So your Makefile can build *on the outpost* instead of shipping
images over the wire:

```sh
podman system connection add outpost ssh://$USER@outpost.local
```

This rides outpost's existing SSH endpoint (no extra port to open).
See [`remote-podman.md`](remote-podman.md) for the full transport
story.

### 4. The Makefile

```make
APP    := myapp
TAG    := dev
PORT   := 18080
REMOTE := outpost.local

test:
	go test ./...

deploy: test
	podman --connection=outpost build -t localhost/$(APP):$(TAG) .
	outpost --remote $(REMOTE) apps stop $(APP)-dev || true
	-podman --connection=outpost rm -f $(APP)-dev
	podman --connection=outpost run -d --name $(APP)-dev \
		--restart unless-stopped \
		-p $(PORT):8080 localhost/$(APP):$(TAG)
	outpost --remote $(REMOTE) apps add --name $(APP)-dev \
		--url http://127.0.0.1:$(PORT)
	outpost --remote $(REMOTE) apps start $(APP)-dev
```

The `apps stop` → swap → `apps start` sandwich is optional —
`outpost apps add` is idempotent, the container replacement is fast,
and most reloads don't need a drain. Use it when the change is bigger
(port number changed, image format swap) and you'd rather see clean
503s than flapping 502s.

## `outpost apps stop` / `outpost apps start`

These flip the *proxy gate*, not the upstream. Stopping an app makes
the cloudbox-side tile (and the loopback proxy at `/app/<name>`)
respond as if the app were removed — but the underlying container or
process keeps running. To stop the upstream, run `podman stop
<name>` (or `systemctl stop …`) separately.

When you want both:

```sh
outpost apps stop myapp-dev          # cloudbox starts 503'ing
podman stop myapp-dev                # upstream goes down
```

`apps stop` survives daemon restarts (the Enabled=false flag is
persisted). To re-enable: `outpost apps start myapp-dev`.

## Troubleshooting

- **"outpost daemon at outpost.local not reachable"** — the admin
  listener isn't bound to a reachable address, or outpost isn't
  running. SSH into the host and check `outpost status`.
- **"no MCP bearer token"** — `outpost mcp endpoint` reads it from
  `agent.json`. If empty, run `outpost start` once on the daemon
  machine; the token is auto-generated on first boot.
- **`podman --connection=outpost`: permission denied** — the OS user
  on the outpost host doesn't own the podman socket. Match the
  `ssh://$USER@host` username to the user that runs podman there.
- **App tile in cloudbox shows 502/503 after deploy** — check the
  container's port mapping (`podman port <name>`) matches the
  `--url` you passed to `outpost apps add`. The mapping is host
  port → container port; the AppConfig URL is the host port.
- **`apps stop` doesn't free the port** — by design. The container
  keeps the port bound. Stop the container too if you need the port.

## What this does NOT cover

- **Push-driven deploys** (GitHub/Gitea webhooks, CI runners) — a
  separate iteration. Existing primitives are sufficient if you wire
  a self-hosted runner on the outpost host; see [`mcp.md`](mcp.md)
  for the agent-tool surface that orchestration would call.
- **Plain processes / k8s** — same Makefile shape, swap the deploy
  step. Plain processes use `scp` + `systemctl --user restart`;
  k8s uses `helm upgrade --install` against the kubeconfig outpost
  auto-writes at `~/.kube/outpost.yaml` when cluster mode is on.
