# Pairing a host with cloudbox

This is the end-to-end runbook for turning a machine that has the
`outpost` binary into a **paired host** — one cloudbox can reach through
the matrix tunnel and that shows up in `outpost peers status`. It is
written so a human *or* an agent can follow it verbatim.

A paired host is reachable two ways, and you want to keep them straight:

- **Through cloudbox** (`outpost connect <host>` → `outpost ssh <host>`):
  the durable path. Survives reboot once the boot service is installed.
- **LAN-direct** (`outpost ssh user@<host>.local:2222`, the standalone
  `outpost sshd`): a setup/bootstrap channel. Handy for driving the
  pairing on a remote machine before it is paired — but it is **not**
  durable unless you explicitly make it so (see "Reboot durability").

## TL;DR

```bash
# On the machine being paired (or over a LAN ssh to it):
outpost register --server https://ai.dhnt.io --code <CODE> --name <host> --yes
```

`register` is idempotent against a running daemon: it **merges** the new
pairing into the existing `agent.json` (preserving the daemon's MCP
token, your apps, outbound mounts, builtin toggles, networking, and
admin_users) and, if a daemon is already running, signals it to restart
so it picks up the tunnel. You do not need to stop/kill anything by hand.

Then verify from any already-paired host on the same account:

```bash
outpost peers status        # wait for <host> to show `online`
```

## Step 1 — get a one-time pairing code

A code is minted by cloudbox and bound to **your account**, so the host
you pair ends up owned by you. Two ways to get one:

- **From an already-paired host on the same account** (no browser):

  ```bash
  outpost peers help-mint-invite
  # → Code: <CODE>   Expires: ... (~30 min)
  ```

- **From the cloudbox web UI**: open the portal, "Generate invite code".

Codes are single-use and short-lived (~30 min) — mint it right before
you use it.

## Step 2 — run `register` on the target

The command is the same on every platform; only how you *reach* the
target differs.

**Locally on the target machine:**

```bash
outpost register --server https://ai.dhnt.io --code <CODE> --name <host> --yes
```

`--name` defaults to the machine's hostname (`.local`/`.lan` stripped);
`--server` defaults to the official portal. `--yes` starts the daemon
immediately on a fresh host (skips the interactive "Start now?" prompt).
A **recovery code** is printed and stashed at
`~/.config/matrix/recovery_code.txt` — save it; it is the only
out-of-band way to re-pair if both the access token and the tunnel are
ever lost.

**Driving a remote target over its LAN ssh** (the bootstrap case — the
target only has the binary and an `outpost sshd` on :2222):

```bash
OUTPOST_SSH_PASSWORD='<os-password>' \
  outpost ssh <user>@<host>.local:2222 \
  'outpost register --server https://ai.dhnt.io --code <CODE> --name <host>'
```

(The `.local` + explicit `:2222` forces the LAN-direct path — no
cloudbox detour. `OUTPOST_SSH_PASSWORD` is the non-interactive form of
the OS-password prompt.)

## Step 3 — verify

```bash
outpost peers status
```

Wait for the new host to flip to `online`. The tunnel dials a few
seconds after the daemon (re)starts; cloudbox marks the host online on
its first `/apps` heartbeat. Once online:

```bash
outpost connect <host>      # elevate + cache the matrix_elev cookie
outpost ssh <host> whoami   # now routed through cloudbox
```

## Reboot durability

Pairing data lives in `agent.json` and is durable on disk. What you
must also ensure is that the **daemon relaunches on boot**. Install the
boot service once:

```bash
outpost service install      # registers `outpost supervisord` with
                             # launchd / systemd / Task Scheduler
```

The boot service runs `outpost supervisord`, which keeps `outpost start`
alive (and respawns it across config-change restarts and upgrades). On a
host where this is installed, a reboot brings the tunnel back with no
login required, and the host returns to `online` on its own.

The standalone `outpost sshd` on :2222 used for bootstrapping is **not**
part of the boot service — it is intentionally ephemeral. If you want a
durable LAN-direct SSH as well, set the daemon's `ssh_listen_addr`
instead (it lives inside `outpost start`, so the boot service covers it):

```bash
outpost config set --ssh-listen-addr :2222
```

## Platform notes

- **Windows**: the daemon and the boot task run under a `BootTrigger` +
  S4U scheduled task (no interactive login needed). When you drive
  `register` over `outpost ssh`, the remote exec channel is outpost's
  in-process shell (coreutils), **not** `cmd.exe` — Windows tools
  (`tasklist`, `schtasks`, `netstat`) are not on its PATH. Reach them
  with a full path or via PowerShell, e.g.
  `C:/Windows/System32/WindowsPowerShell/v1.0/powershell.exe -NoProfile -Command "..."`.
- **macOS / Linux**: the boot service is a launchd LaunchAgent/Daemon or
  a systemd unit respectively; `outpost service install` picks the right
  one.

## Offline / installer provisioning

To bake a pairing into a host *before* its daemon ever starts (e.g. an
imaging script), `register` works with no daemon running — it writes the
merged config and exits. The next `outpost start` (or the boot service)
reads it. A few mutate subcommands also accept `--offline` to edit
`agent.json` directly without going through the daemon.

## Troubleshooting

- **`unknown host` from `outpost connect`** — the host is not paired (or
  not under that name / not on your account). Run `outpost peers status`
  to see what cloudbox actually knows; re-check `--name`.
- **Host stuck `offline` after register** — confirm the daemon is
  actually running (`outpost status` on the host) and that
  `agent.json` has a non-empty `access_token`. If a daemon was running
  *before* you paired, `register` should have restarted it
  automatically; if it printed a warning that the restart could not be
  triggered, run `outpost restart` on the host.
- **Lost both the access token and the tunnel** — re-pair out-of-band
  with the recovery code:
  `outpost register --recovery-code <CODE> --name <host>`.
