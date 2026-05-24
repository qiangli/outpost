# Remote podman over outpost

Drive a paired outpost's podman daemon from a different machine, using
podman's native `ssh://` URL transport over outpost's SSH outbound
mount. No daemon HTTP exposure, no DOCKER_HOST tricks — `podman
--connection=<host>` Just Works, including `exec`, `attach`, and `logs
-f`.

## Wire path

```
machine A (local)                cloudbox            machine B (remote)
   podman --connection=B ps
   └─► ssh 127.0.0.1:<port>
        └─► outpost (A) ssh-outbound listener
             └─► wss /h/B/ssh  ──►  outpost (B) /ssh
                                       └─► in-process SSH server
                                            └─► direct-streamlocal@openssh.com
                                                 └─► /run/user/<uid>/podman/podman.sock
```

The local outpost handles the TLS + bearer + `matrix_elev` cookie hop
to cloudbox; the remote outpost handles the OS-password gate and the
unix-socket forwarding.

## Prerequisites

- Both machines paired with the same cloudbox account (`outpost
  register …` or admin-UI flow).
- Remote machine running podman with a reachable socket. Outpost
  auto-detects:
  - Linux rootless: `/run/user/<uid>/podman/podman.sock`
  - Linux root: `/run/podman/podman.sock`
  - macOS: `~/.local/share/containers/podman/machine/podman.sock`
    (or `podman-machine-default` variant)

  Run `podman info` on the remote to confirm the socket is up.

- Local machine running podman (or just the podman CLI) — needs to be
  recent enough to support `system connection add`.

## Setup

### 1. On the remote outpost (B) — nothing to do

The built-in SSH server is on by default (`fc.SSHOn()`), and the
podman socket paths above are pre-allowed for unix-socket forwarding.
You don't need to enable the "Podman" toggle in the admin UI — that
toggle controls the *HTTP* proxy at `/app/podman/`, which is a
different path. SSH-based access doesn't need it.

If the remote socket lives somewhere outpost doesn't auto-detect
(custom podman build, rootful socket on macOS via a custom unit, etc.),
add the absolute path to `ssh_forward_sockets` in the remote outpost's
`agent.json` and restart outpost.

### 2. On the local outpost (A) — add an SSH outbound

```sh
# Register the mount.
outpost outbound add B \
    --scheme=ssh \
    --host=B \
    --user=$REMOTE_USER \
    --local-port=2222

# One-time admin-UI session (1h TTL).
outpost outbound login                # prompts for LOCAL OS password

# Mint the matrix_elev cookie (4-min pinger keeps it warm).
outpost outbound connect B            # prompts for REMOTE OS password
```

This binds `127.0.0.1:2222` on machine A; every TCP connection to that
port is byte-bridged via WSS to machine B's built-in `/ssh` endpoint.

### 3. On machine A — register the connection with podman

```sh
podman system connection add B \
    ssh://$REMOTE_USER@127.0.0.1:2222/run/user/$(ssh -p 2222 $REMOTE_USER@127.0.0.1 'id -u')/podman/podman.sock
```

Or just type the socket path explicitly if you already know it:

```sh
podman system connection add B \
    ssh://joe@127.0.0.1:2222/run/user/1000/podman/podman.sock
```

The first `ssh` invocation will prompt for the remote host key — accept
it (it's outpost B's persistent ed25519 key under `<UserConfigDir>/matrix/ssh_host_ed25519`).

## Use

```sh
podman --connection=B ps
podman --connection=B run --rm alpine echo hello
podman --connection=B exec -it <ctr> sh
podman --connection=B logs -f <ctr>
```

Make it the default:

```sh
podman system connection default B
podman ps                   # talks to B
```

## Custom socket allowlists

To allow a non-default socket (e.g., a development podman, a unix
socket exposed by a sibling tool), add it to `agent.json` on the
**remote** machine:

```json
{
  "ssh_forward_sockets": [
    "/run/user/1000/my-custom.sock"
  ]
}
```

Entries are absolute paths, exact-matched after `filepath.Clean`. No
globbing. Restart outpost on the remote after editing.

## Why not docker

Docker's `DOCKER_HOST=ssh://…` works against outpost today *without*
this feature, because docker uses the SSH `exec` channel to run `docker
system dial-stdio` on the remote — outpost's SSH server has supported
exec channels from day one. You just need the docker CLI installed on
the remote outpost:

```sh
docker -H ssh://joe@127.0.0.1:2222 ps
```

(`exec` channel → remote runs `docker system dial-stdio` → bytes back
to the local docker CLI.)

The `direct-streamlocal@openssh.com` channel added here is specifically
what podman's `ssh://` transport requires — it forwards a remote unix
socket directly, which is faster and doesn't require any podman CLI on
the remote.

## Troubleshooting

- **`channel open failed: unknown channel type`** — the remote outpost
  is older than this feature. Update it.
- **`channel open failed: socket not in allowlist`** — the path you
  asked for isn't in the default podman/docker set and isn't in
  `ssh_forward_sockets` on the remote. Add it and restart the remote
  outpost.
- **`channel open failed: dial /…: no such file or directory`** — the
  socket path is allowlisted but the daemon isn't running. Check
  `podman info` / `systemctl --user status podman.socket` on the
  remote.
- **`Host key verification failed`** — the remote's host identity
  changed (rare; the ed25519 key is persisted across re-pairings).
  Either trust the new key (`ssh-keygen -R '[127.0.0.1]:2222'` then
  retry) or investigate why the remote's
  `<UserConfigDir>/matrix/ssh_host_ed25519` is gone.
- **`outpost outbound connect` returns 401/403** — the elevation
  cookie expired or the remote OS password was wrong. Re-run
  `outpost outbound connect B`.
