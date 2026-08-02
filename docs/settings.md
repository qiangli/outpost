# Outpost settings reference

This is the canonical inventory of every persistable setting in
outpost — where it lives, how to read it, how to write it, and what
takes effect when.

Every setting has at most four surfaces:

- **File**: `~/.config/matrix/agent.json` on every platform
  (XDG-style; mode 0600, auto-generated on first boot). Honors
  `$XDG_CONFIG_HOME` when set. On Windows this resolves to
  `C:\Users\<user>\.config\matrix\agent.json`. Cache files live under
  `~/.cache/outpost/` with the same convention (`$XDG_CACHE_HOME`
  honored). Older installs that wrote agent.json under
  `~/Library/Application Support/matrix/` (macOS) or `%AppData%\matrix\`
  (Windows) are auto-migrated on the next `outpost start`; the older
  copy is renamed to `*.bak.<ts>` so nothing is silently lost.
- **CLI**: a `outpost <verb> [...]` invocation (cobra subcommand or
  flag on `outpost start` / `outpost register`).
- **UI**: a field or toggle in the local admin SPA at
  `http://127.0.0.1:17777`.
- **MCP**: a tool name on the MCP server at `/mcp/*` of the same
  listener. Driven by agent tools (Claude Code, Windsurf, …) via
  `.mcp.json`.

## Precedence

For settings that double as boot-time arguments, the precedence in
`outpost start` is:

```
CLI flag  >  env var  >  agent.json  >  hardcoded default
```

This is deliberate. `conf.Load()` no longer bakes hardcoded defaults
into env-empty fields — that would mask file lookups. The
package-level defaults live in `internal/agent/conf/conf.go` (the
`Default*` constants) and are applied last.

## Side-effect classes

- **Restart**: the daemon re-execs to bring the change live. UI shows
  "Restarting…" and the CLI prints "Restarting outpost — poll
  `outpost status`". On a fresh, unpaired host (no AgentName) the
  restart is skipped — nothing is mounted yet.
- **Live**: change takes effect on the next request, no restart
  needed. Custom apps and outbound mounts are live-mutable.
- **Boot-only**: change persists but only takes effect on the next
  `outpost start`. The matrix-tunnel pairing fields and network
  binds are boot-only (the tunnel client is built once at boot).

## Naming convention

The file key is the canonical name. The other surfaces follow:

- **File** (`agent.json`): `snake_case`, e.g. `ssh_allow_local_forward`.
- **MCP** tool arg: identical to the file key.
- **CLI** flag: kebab-case of the file key, e.g.
  `--ssh-allow-local-forward`. A few historical short spellings
  (e.g. `--ssh-local-fwd`) survive as deprecated aliases that print
  a one-line warning.
- **UI** label: human English with the canonical file key shown as
  a small subtext code-block so an operator can match concepts up
  visually when moving between surfaces.

CLI subcommand verbs (`add`, `rm`, `list`) intentionally stay
Unix-conventional. MCP tool names use database-style verbs
(`outpost_upsert_app`, `outpost_delete_app`). Both are fine — the
audiences differ — and the mapping is one-to-one.

## Inventory

### Pairing identity (portal-controlled)

| Field | File key | CLI | UI | MCP | Effect |
|---|---|---|---|---|---|
| Agent name | `agent_name` | `register --name` (alias: `outpost pair`) | Pair tab | `outpost_pair` | Restart |
| Portal server | `server_addr` / `server_port` / `protocol` | `register --server`, `$MATRIX_SERVER_ADDR`, `$MATRIX_SERVER_PORT`, `$MATRIX_PROTOCOL` | Pair tab (display only) | `outpost_pair` | Restart |
| Tunnel token | `token` | (portal-issued; never user-input) | `has_token` flag only | (never exposed) | Restart |
| Cloudbox access token | `access_token` | (portal-issued) | `has_token` flag only | (never exposed) | Restart |
| Remote port | `remote_port` | (portal-issued; `$MATRIX_REMOTE_PORT` override) | display | (never exposed) | Restart |
| External auth URL | `auth_url` | `register --auth-url`, `$MATRIX_AUTH_URL` | Pair tab | `outpost_pair` | Restart |
| Client-only mode | `client_only` | `register --client-only` | Pair tab (display only — re-pair to change) | `outpost_pair` | Restart |

Pairing always goes through `portal.Exchange` (cloudbox issues
`token` + `access_token` + `remote_port`). `register` and
`outpost_pair` are the same code path; `register` runs daemon-less
so installer scripts can provision before `start`.

To clear a pairing: `outpost unpair --yes` (CLI), the equivalent MCP
tool, or wipe `agent.json` by hand.

### Built-in routes (boot-time-bound)

| Field | File key | CLI | UI | MCP | Effect |
|---|---|---|---|---|---|
| Shell | `shell_enabled` | `builtins set --shell` | Inbound > Built-ins | `outpost_set_builtins` | Restart |
| Desktop (VNC) | `desktop_enabled` | `builtins set --desktop` | Inbound > Built-ins | `outpost_set_builtins` | Restart |
| Clipboard | `clipboard_enabled` | `builtins set --clipboard` | Inbound > Built-ins | `outpost_set_builtins` | Restart |
| SSH | `ssh_enabled` | `builtins set --ssh` | Inbound > Built-ins | `outpost_set_builtins` | Restart |
| SSH `-L` local-fwd | `ssh_allow_local_forward` | `builtins set --ssh-allow-local-forward` (alias: `--ssh-local-fwd`) | Inbound > Built-ins | `outpost_set_builtins` | Restart |
| SSH `-R` remote-fwd | `ssh_allow_remote_forward` | `builtins set --ssh-allow-remote-forward` (alias: `--ssh-remote-fwd`) | Pair tab > Advanced | `outpost_set_builtins` | Restart |
| SSH `-A` agent-fwd | `ssh_allow_agent_forward` | `builtins set --ssh-allow-agent-forward` (alias: `--ssh-agent-fwd`) | Pair tab > Advanced | `outpost_set_builtins` | Restart |
| SSH forward-sockets allowlist | `ssh_forward_sockets` | `builtins set --ssh-forward-socket /path ...` | Pair tab > Advanced | `outpost_set_builtins` | Restart |
| SFTP subsystem | `sftp_enabled` | `builtins set --sftp` | Inbound > Built-ins | `outpost_set_builtins` | Restart |
| File Browser | `files_enabled` | `builtins set --files` | Inbound > Built-ins | `outpost_set_builtins` | Restart |
| File Browser write access | `files_allow_write` | `builtins set --files-allow-write` | Inbound > Built-ins > File Browser | `outpost_set_builtins` | Restart |
| File Browser scope (root) | `files_scope` | `builtins set --files-scope <path>` | Inbound > Built-ins > File Browser | `outpost_set_builtins` | Restart |
| Podman daemon proxy (raw) | `podman_enabled` | `builtins set --podman` | Inbound > Built-ins | `outpost_set_builtins` | Restart |
| Container sandbox (filtered) | `sandbox_enabled` | `builtins set --sandbox` | Inbound > Built-ins | `outpost_set_builtins` | Restart |
| Ollama daemon proxy | `ollama_enabled` | `builtins set --ollama` | Inbound > Built-ins | `outpost_set_builtins` | Restart |
| Ollama LLM-pool participation | `ollama_pool_enabled` | `builtins set --ollama-pool` | Inbound > Built-ins | `outpost_set_builtins` | Restart |
| Warm serving (considerate) | `warm_serving_enabled` | `builtins set --warm-serving` | Inbound > Built-ins | `outpost_set_builtins` | Restart |
| Warm budget fraction | `warm_budget_frac` | `builtins set --warm-budget-frac` | Inbound > Built-ins | `outpost_set_builtins` | Restart |
| Warm desired set (persisted) | `warm_desired` | — (managed by `/admin/warm`) | — | — | Live |
| Same-LAN direct inference | `lan_inference_enabled` | `builtins set --lan-inference` | Inbound > Built-ins | `outpost_set_builtins` | Restart |
| Same-LAN direct inference port | `lan_inference_port` | `builtins set --lan-inference-port` | Inbound > Built-ins | `outpost_set_builtins` | Restart |
| Meet web chat room | `meet_enabled` | `builtins set --meet` | Inbound > Built-ins | `outpost_set_builtins` | Restart |
| Meet web chat room port | `meet_port` | `builtins set --meet-port` | Inbound > Built-ins | `outpost_set_builtins` | Restart |
| Cluster join | `cluster.enabled` | `builtins set --cluster` | Inbound > Cluster | `outpost_set_builtins` | Restart |
| Digital Ocean support (cloud venue) | `cloud_do_enabled` | `builtins set --cloud-do` | Inbound > Cloud | `outpost_set_builtins` | Restart |
| Digital Ocean API token | `cloud_do_token` | `builtins set --cloud-do-token` | Inbound > Cloud | `outpost_set_builtins` | Restart |
| Cloudbox-pushed self-upgrade | `update_mode` | `builtins set --update=auto\|manual\|never` | Inbound > Built-ins | `outpost_set_builtins` | Live |
| Auto-rollback watchdog (destructive revert) | `auto_rollback_enabled` | `builtins set --auto-rollback=on\|off` | Inbound > Built-ins | `outpost_set_builtins` | Live |
| Headlamp operating UI (peer DKS plane) | `headlamp_enabled` | `builtins set --headlamp` | Inbound > Built-ins | `outpost_set_builtins` | Restart |
| Headlamp loopback forward port | `headlamp_port` | `builtins set --headlamp-port` | Inbound > Built-ins | `outpost_set_builtins` | Restart |

All built-in toggles default to ON when the JSON key is absent (old
configs) so an upgrade doesn't silently disable features. That now
includes the o3 proxies — `podman_enabled`, `ollama_enabled` and
`otel_enabled` — so a freshly paired host is useful with zero
configuration. They are *detection-gated*: outpost registers each mount
only when the backing daemon is actually reachable, and a host without
podman / Ollama / an observability proxy logs one line and carries on.
o3 is never a dependency; nothing fails to start because it is missing.

The remaining opt-ins (default OFF, explicit choice required) are
`sandbox_enabled` (widens *who* may run containers on the host),
`lan_inference_enabled` (a LAN-trust listener with no per-request auth),
`meet_enabled` (a supervised chat-room service) and `cluster.enabled`
(hands a remote control plane the right to schedule workloads here).

Because the o3 keys are default-ON, "off" is only representable as an
explicit `false` on disk — those three are pointer-bools so an opt-out
is written literally and survives every later config write. Do not
"simplify" them to plain `bool`: with `omitempty` a false marshals to
absent, and the next load would read absent as the default and turn the
service back on.

### Meet web chat room (supervised bashy service)

`meet_enabled` (default **OFF**) runs the **meet** web chat room — a
Slack-style browser UI over `bashy meet` — as a supervised bashy service
on the loopback port `meet_port` (default `8637`), published as a
cloudbox app named `meet` (reachable at `/matrix/h/<host>/app/meet/`,
login required). This is the same generic supervision framework loom
uses: the daemon starts it on boot, health-checks every 30s, restarts on
`stopped`, and stops it on shutdown.

The built-in service entry also sets `trust_cloud_identity:true`. Before
registration, outpost generates a per-service 32-byte `sso_secret` when
needed, persists it in the `bashy_services` entry, and supplies both fields
to the existing signed-header proxy path. Cloud requests therefore reach
meet with `Remote-User` / `Remote-Email` plus
`X-Outpost-Identity-Sig`, which its `coopauth.Guard` can verify. An enabled
trusted service with no secret is rejected during registration instead of
silently running with spoofable unsigned identity headers.

**Lifecycle trap.** The service lives under a subcommand, so the
supervisor drives **`bashy meet service {start,status,stop}`** — NOT
`bashy meet start`, which already exists and starts a *deliberation
session*, not the web server. The `["meet","service"]` Command base is
pinned in `conf.DefaultBashyServices()` so an operator who enables meet
through the `bashy_services` array without re-declaring the argv still
gets the daemon. There is **no mesh service**: a personal chat room has
no peer consumer, so unlike loom it is not auto-exposed over the mesh.
The backing `bashy meet service` is provided by the bashy/coreutils side;
the supervisor tolerates it being absent (the 30s loop retries, and
`bashyResolver` self-heals a missing bashy), so enabling meet before the
subcommand exists is safe — it comes up on the next poll once installed.

Four surfaces: `meet_enabled` / `meet_port` file keys, `builtins set
--meet=on --meet-port N` CLI, the Built-ins > Meet chat UI toggle, and
the `meet` / `meet_port` args on the `outpost_set_builtins` MCP tool.
Like loom, a meet change triggers a **restart** (the service is wired at
boot).

### Headlamp operating UI (peer DKS plane)

`headlamp_enabled` (default **OFF**) deploys [Headlamp](https://headlamp.dev)
(the Kubernetes cluster operating UI) into a peer-hosted DKS control plane.
When on and `cluster.control_plane` is enabled, outpost runs
`headlamp.Deploy` against the plane, supervises the loopback port-forward on
`headlamp_port` (default `18466`), and exposes it over the mesh as the
`headlamp` service. Headlamp runs in token-login mode: the pod's
ServiceAccount holds zero RBAC grants; every apiserver request is
authenticated with the token the operator pastes into the UI.

Four surfaces: `headlamp_enabled` / `headlamp_port` file keys, `builtins set
--headlamp=on --headlamp-port N` CLI, the Built-ins > Headlamp UI toggle,
and the `headlamp` / `headlamp_port` args on the `outpost_set_builtins` MCP
tool. A headlamp change triggers a **restart** (the deployment + forward are
wired at boot). The forward binds loopback only — `headlamp.ValidateListenAddr`
rejects non-loopback binds per the standing rule.

See `docs/peer-dks-headlamp.md` for the complete security model, auth
boundary, and verification procedure.

### Same-LAN direct inference (LAN-trust)

`lan_inference_enabled` binds a **LAN-reachable** listener on
`0.0.0.0:<lan_inference_port>` (default `11435`, kept distinct from the
inference server's own `11434`) that reverse-proxies the OpenAI `/v1/*`
and Ollama `/api/*` surface to the local inference server at
`127.0.0.1:11434` (Ollama, or — when a shard is active — the shard
leader's llama-server, which also serves the OpenAI `/v1` API on 11434).
A caller on the same LAN can then reach this host's LLM **directly**,
bypassing the cloudbox relay for lower latency. When on (and paired),
the outpost also advertises the endpoint to cloudbox in the LLM-pool
registry push as `lan_endpoint`
(`http://<primary-private-LAN-IPv4>:<lan_inference_port>/v1`), so
cloudbox can hand it to callers it detects on the same LAN.

**This is a LAN-TRUST endpoint. It is NOT authenticated per-request.**
Enabling it means the operator acknowledges their LAN is trusted —
anyone who can reach the port can use the local inference server.
Untrusted or shared/org networks should leave `lan_inference_enabled`
**off** and use the Bearer-authed cloudbox `/v1` gateway instead (which
authenticates every request). The toggle requires the local Ollama proxy
on and pairing (cloudbox is what advertises the LAN endpoint to same-LAN
callers). A bind failure is non-fatal — it logs and degrades without
taking the daemon down.

### Warm serving (adaptive, considerate, always-on)

`warm_serving_enabled` (default **ON** for a paired Ollama node) runs the
considerate warm-serving plane: the outpost keeps a small, conservative
set of LLM models **warm** (resident, zero cold-start) but **yields** the
machine to the user's own work the moment they get busy — unloading warm
models when system load is high and restoring them when the host goes
idle again.

Two moving parts:

- A **system-load profiler** samples CPU utilization, available memory,
  and the load average every ~30 s, and learns a rolling **per-hour-of-
  day baseline** of "normal" load over days (persisted to
  `<cacheDir>/outpost/sysload.json`, so it survives restarts). It reports
  the host as **busy** when the current load is meaningfully above this
  hour's baseline **or** above absolute safety thresholds. Defaults:
  sustained CPU **>60%**, or available memory **<25%**. The verdict is
  **debounced** — a condition must hold ~2 min to enter busy, and the
  host must be quiet ~5 min to leave it — so a brief spike never yanks
  warm models.
- The **warm budget** is the conservative memory the host will dedicate
  to warm preload: `warm_budget_frac` × usable memory (default **0.33**,
  leaving ~2/3 for the OS and the user's apps — e.g. ~10 GB on a 32 GB
  host). It drops to **0 whenever the host is busy**, so the host fully
  yields. `warm_budget_frac` is clamped to `(0, 1]`.

The **desired warm set** (`warm_desired`) is the list of models cloudbox
last asked this host to keep warm. A supervisor loop unloads that set
while the host is busy and restores it (within the current budget) once
idle — so even mid-request the host protects the user's other work and
self-heals when quiet. `warm_desired` is persisted but managed by the
control endpoint, not edited directly.

Each LLM-pool registry push advertises the live signal to cloudbox:
`warm_budget_bytes` (0 when busy) and `busy`. These are advisory — the
control endpoint below re-checks the live budget before acting.

**Control endpoint — `POST /admin/warm`** (cloudbox → outpost through the
matrix tunnel; same tunnel-as-auth-boundary trust as `/admin/upgrade`, no
bearer at the HTTP layer, mounted only on paired hosts). Body:

```json
{ "model": "<name>", "mode": "load" | "shard" | "unload" }
```

- `load` — make the model resident with a persistent keep-alive
  (`keep_alive: -1`), pulling it first if missing. Idempotent.
- `shard` — ensure a shard is formed+serving the model (reuses the shard
  manager). Idempotent — a no-op if it's already the active shard model.
- `unload` — release the model (`keep_alive: 0`) and tear down the shard
  if it's the active one.

A `load`/`shard` that would exceed the warm budget (or arrives while the
host is busy) is skipped, not forced. Reply:

```json
{ "status": "...", "active_model": "...", "busy": false, "warm_budget_bytes": 0 }
```

where `status` is one of `loaded` / `already_resident` / `shard_started`
/ `already_active` / `unloaded` / `skipped_busy` / `over_budget`.

### Container sandbox provider

`sandbox_enabled` exposes a **filtered** container endpoint at
`/app/sandbox/`, distinct from the raw `/app/podman/` passthrough. It
shares the same podman socket `DetectPodman()` finds, but every
`containers/create` and exec-create request is vetted: privileged
containers, host network / PID / IPC / UTS / user / cgroup namespaces,
host bind mounts (outside `sandbox_scratch_dir`), added capabilities,
device passthrough, and confinement-disabling `security-opt` /
`selinux_opt` are all rejected with a `403 {"message":"sandbox: …"}`.
This is the mount a thin client or an untrusted tenant talks to;
`/app/podman/` stays admin-only for trusted self-use.

The optional resource policy (all default 0 = "no explicit limit", the
filter then leaves the caller's value untouched):

| Setting | File key | Meaning |
|---|---|---|
| Sandbox memory cap (MiB) | `sandbox_max_memory_mb` | per-container memory ceiling; clamps a larger request down |
| Sandbox CPU cap (cores) | `sandbox_cpus` | per-container CPU cap (docker NanoCpus) |
| Sandbox PIDs cap | `sandbox_pids_limit` | per-container process cap (fork-bomb defense) |
| Sandbox max containers | `sandbox_max_containers` | advertised concurrency ceiling (capacity report) |
| Sandbox image allowlist | `sandbox_allowed_images` | exact refs or `repo/*` wildcards; empty = any image |
| Sandbox scratch dir | `sandbox_scratch_dir` | sole host path prefix under which bind mounts are allowed; empty = forbid host binds (named volumes/tmpfs always ok) |
| Sandbox prewarm images | `sandbox_prewarm_images` | images the prewarmer keeps pulled so a remote create+start skips the pull cost; empty falls back to the concrete (non-wildcard) `sandbox_allowed_images` entries |

These policy fields are edited in `agent.json` directly (or via
`outpost_set_builtins` / the SPA); only the `sandbox_enabled` toggle has
a dedicated CLI flag. Like the other daemon proxies, flipping
`sandbox_enabled` triggers a restart because it (un)registers an app at
boot. Cloudbox discovers sandbox-bearing hosts via the `/apps`
capability advertisement (`{type:"sandbox"}`) and can probe
`/app/sandbox/_pool/capacity` for load-aware routing.

The **File Browser** builtin (`files_enabled`, default **on**) embeds a
File Browser SPA in-process — the GUI sibling of `/shell` and `/ssh` for
browsing and downloading files — registered as the `files` app so it
rides the same per-app gate (`require_login` + cloudbox elevation). It is
**read-only + download-only by default**: `files_allow_write` flips every
write op (upload/edit/rename/delete) together. The embed is **single-user
and stateless** — no database. Scope and write mode come from config; every
write to the in-process user no-ops, so a cloud-vouched user can never turn
a read-only browser into a writable one (write-enable is a config/LAN
decision, not an in-app one). `files_scope` confines the browser to a
directory (empty = the OS user's home). Per-user UI preferences (view mode,
hide-dotfiles, language, …) are kept **in the browser's localStorage**, so
they are per-device and never shared between users of a shared instance.
`files_signing_key` is the only persisted File Browser state (see the
daemon-internal secrets table). All flip a restart (the handler mounts at
boot).

`update_mode` is the only built-in setting with **Live** effect — the
upgrade worker re-reads the FileConfig on each `POST /admin/upgrade`,
so flipping it doesn't require (and doesn't trigger) a restart. Three
values, default **auto** for paired hosts:

- **`auto`** — incoming envelopes are staged + probed + swapped +
  daemon re-execs. The "press button, fleet rolls" behavior.
- **`manual`** — daemon validates the envelope, persists it to
  `<cacheDir>/outpost/upgrade.pending.json`, returns 202
  `pending_manual`, and does NOT swap. The cloudbox UI shows an
  "Update pending — Apply" badge; the operator triggers the swap
  by clicking Apply (which re-POSTs the envelope with `force: true`
  to bypass the manual gate) or by running `outpost upgrade apply`
  on the host. Use case: cautious operators who want notification
  but not auto-application.
- **`never`** — daemon returns 403 `disabled`. Even Force=true is
  refused — the operator must flip the mode first. Use case: a
  frozen box you fully control (debugging session, regression
  bisection, compliance freeze).

Migration: legacy `auto_upgrade: true` is read as `update_mode: auto`;
`auto_upgrade: false` is read as `update_mode: never`. New writes
clear `auto_upgrade` and persist only `update_mode`. The deprecated
`--auto-upgrade=on|off` CLI flag survives as an alias.

Unpaired hosts ignore the setting — the `/admin/upgrade` route only
mounts once cloudbox has issued an `access_token`.

#### Cloudbox-pushed upgrade flow

When `update_mode` is `auto` (or `manual` with `force: true` in the
envelope), cloudbox POSTs to `<this-host>/admin/upgrade`
through the matrix tunnel. No `Authorization` header — the route trusts
the tunnel as the auth boundary, the same model `/apps` and `/healthz`
already use. The daemon's main HTTP server binds 127.0.0.1 only, so
cloudbox-via-tunnel is the only party that can reach the route.
Defense-in-depth lives at the worker layer: the `auto_upgrade` toggle
(operator opt-out), the sha256 + envelope.commit integrity checks, and
the Probe step (`<candidate> version --json` must self-report
envelope.commit). The envelope is shaped like:

```json
{
  "release_id": "v0.42.1-abc1234",
  "url": "https://releases.ai.dhnt.io/outpost/<sha>/outpost-darwin-arm64",
  "sha256": "<hex>",
  "commit": "abc1234",
  "min_from": "0f572aa"
}
```

The daemon downloads the binary (HTTPS, sha256-verified), execs the
candidate with `version --json` to confirm its self-reported commit
matches the envelope, hardlinks the live binary to
`<binary>.previous` (one-generation rollback retention), atomically
renames the candidate over the live path, and triggers a self-restart.
Each phase emits one JSONL entry to `<cacheDir>/outpost/upgrade.log`,
viewable via `outpost upgrade history` or the `outpost://upgrade-history`
MCP resource. Failed phases abort the swap without touching the live
binary.

Rollback: `outpost rollback` swaps `<binary>.previous` back over the
live binary and restarts. After rollback the previous file is gone —
re-upgrade if you want to climb forward again.

Auto-rollback watchdog (`auto_rollback_enabled`, default **off**): after
a self-upgrade swap, the daemon leaves a confirmation marker
(`upgrade-pending-confirm.json`). The new binary "confirms healthy" by
simply **staying up** for a dwell period — a purely local signal that
needs no cloudbox round-trip, so a good binary that boots during a WAN
outage still self-confirms and is never falsely reverted. If instead the
new binary crash-loops (never stays up long enough), the supervisor —
the always-up parent that survives a crash-loop — reverts
`<binary>.previous` on the next respawn and **quarantines** the bad
`release_id` so the pull-trigger doesn't re-apply it (clear with `outpost
upgrade unquarantine`). The destructive revert is gated by
`auto_rollback_enabled`: when **off** (the default), the watchdog only
*observes* — it logs `watchdog: WOULD auto-rollback …` so you can
validate the signal on a canary before arming it fleet-wide. A revert is
refused (binary left in place) if `<binary>.previous` itself fails to
probe, so a double-brick never makes things worse. Ledger steps:
`confirm_ok`, `auto_rollback`, `auto_rollback_failed`.

Status codes the daemon returns to cloudbox:

| HTTP | Status | Meaning |
|---|---|---|
| 202 | accepted | upgrade staged + worker goroutine running |
| 200 | replay | same `release_id` already applied (idempotent; remembered across the post-swap restart via the upgrade ledger) |
| 409 | in_flight | another upgrade is currently running |
| 304 | same_commit | daemon is already at this commit |
| 403 | disabled | operator set `update_mode` to `never` |
| 412 | min_from | daemon's current commit is older than `min_from` requires |
| 409 | quarantined | this `release_id` was auto-reverted on this host; refused until cleared or superseded |
| 400 | (invalid envelope) | required field missing or `url` is not https |

### Apps (live)

`apps[]` is a slice of `AppConfig`. Each entry:

| Field | File key | CLI flag on `apps add` | UI input |
|---|---|---|---|
| Name | `name` | (positional arg) | Name |
| Icon URL | `icon` | `--icon` | Icon URL |
| Scheme | `scheme` | `--scheme` | Protocol dropdown |
| Host | `host` | `--host` | Host |
| Port | `port` | `--port` | Port |
| Socket | `socket` | `--socket` | (unix/npipe only) |
| URL (single-string alt.) | (parsed into the above) | `--url` | (n/a) |
| Enabled | `enabled` | `--disabled` inverts; flip later with `apps stop` / `apps start` | Toggle |
| Require login | `require_login` | `--require-login` | Checkbox |
| LAN-only paths | `lan_only_paths` | `--lan-only-path /p` (repeatable) | Textarea |
| Index path | `index_path` | `--index-path` | Index path |
| Trust cloud identity | `trust_cloud_identity` | `--trust-cloud-identity` | Checkbox |
| Provisioning token | `provisioning_token` | auto-generated; rotate with `apps rotate-token` | Reveal / Copy / Rotate |

MCP equivalents: `outpost_upsert_app`, `outpost_delete_app`,
`outpost_set_app_enabled`, `outpost_rotate_app_token`,
`outpost_suggest_apps`.

App add / update is **live** — the running `AppRegistry` is mutated
under a mutex, no restart needed. App removal is also live.
`apps stop` / `apps start` (and `outpost_set_app_enabled`) flip
only the proxy gate — the upstream container/process is untouched.

### Outbound mounts (live)

`outbound[]` is a slice of `OutboundConfig`. Each entry:

| Field | File key | CLI flag on `outbound add` | UI input |
|---|---|---|---|
| Local path / identifier | `path` | `--path` | Path |
| Remote app name | `name` | `--name` | (auto from dropdown) |
| Remote host | `host` | `--host` | (auto from dropdown) |
| Remote OS user | `user` | `--user` | (auto from dropdown) |
| Scheme | `scheme` | `--scheme` (`http`, `tcp`, `ssh`) | Scheme |
| Local port | `local_port` | `--local-port` | Port (tcp/ssh only) |
| TTL override | `ttl_seconds` | `--ttl` | TTL selector |

MCP equivalents: `outpost_upsert_outbound`, `outpost_delete_outbound`,
`outpost_connect_outbound`, `outpost_disconnect_outbound`,
`outpost_suggest_outbound`.

Add / remove / connect / disconnect are all **live**. `connect`
requires the user's OS password on the remote host (human-in-the-loop
for agent calls).

### Cluster join (k3s-agent default)

| Field | File key | CLI | UI | MCP |
|---|---|---|---|---|
| Joined | `cluster.enabled` | `builtins set --cluster` / `cluster clear` | Inbound > Cluster | `outpost_set_builtins` / `outpost_clear_kubeconfig` |
| Real agent node | `cluster.runtimes.agent` | `builtins set --cluster-agent on|off` | Inbound > Cluster | `cluster_agent` |
| Virtual nodes | `cluster.runtimes.virtual` | `builtins set --cluster-virtual vk-native,vk-podman` | Inbound > Cluster | `cluster_virtual` |
| Apiserver URL | `cluster.api_url` | (fetched from cloudbox at boot) | display | (auto-fetched) |
| Bearer token | `cluster.token` | (fetched from cloudbox at boot) | `has_token` flag only | (auto-fetched; never read back) |
| CA bundle | `cluster.ca` | (fetched from cloudbox at boot) | `has_ca` flag only | (auto-fetched; never read back) |
| Node name override | `cluster.node_name` | (set in cloudbox's host record) | display | (managed in cloudbox) |

Save = restart (the cluster runtimes are built once at boot). A host may
run one real k3s-agent Node and one Node for each selected virtual-kubelet
backend concurrently. Virtual node names are `<node>-vk-native`,
`<node>-vk-podman`, and `<node>-vk-ollama`.

### Control-plane placement

Whether this host **hosts** the apiserver rather than merely joining one.
The apiserver can live on cloudbox, on a rented always-on box, or on this
machine — workers dial a tunnel server the same way in every case, so
switching placement is a configuration change, not a migration.

| Field | File key | CLI | UI | MCP |
|---|---|---|---|---|
| Host the apiserver | `cluster.control_plane` | `cluster control-plane on\|off` | Inbound > Cluster | `outpost_set_control_plane` |
| Tunnel bind address | `cluster.tunnel_bind_addr` | `cluster control-plane --bind-addr` | Inbound > Cluster | `outpost_set_control_plane` |
| Tunnel bind port | `cluster.tunnel_bind_port` | `cluster control-plane --bind-port` | Inbound > Cluster | `outpost_set_control_plane` |
| Join token | `cluster.tunnel_token` | `cluster control-plane token` | `has_tunnel_token` flag only | `outpost_control_plane_token` |
| Rotate the join token | `cluster.tunnel_token` | `cluster control-plane token rotate --yes` | — | `outpost_rotate_control_plane_token` |
| k3s node token | (k3s state, not `agent.json`) | `cluster token` | — | `outpost_cluster_node_token` |
| Hosted-plane kubeconfig | `cluster.control_plane_kubeconfig` | (edit agent.json) | — | — |
| Hosted apiserver address | `cluster.control_plane_api_addr` | (edit agent.json) | — | — |

The last two rows have **no CLI/MCP/UI surface yet** — they are set by
editing `agent.json`. That is a known gap, called out here rather than left
for someone to discover.

`cluster token` is the odd one out: the k3s node token is not an outpost
setting at all. k3s mints it inside the control-plane container and writes it
to `/var/lib/rancher/k3s/server/node-token`, which lives in a container VOLUME
— on macOS and Windows, inside podman's VM, so there is no host path to read.
The command reads it through a container exec and prints it to **stdout only**,
never to the daemon log, so it pipes cleanly:

```bash
outpost cluster token | ssh worker outpost cluster join 10.0.0.5 --token-stdin
```

`control_plane_kubeconfig` is what the cluster-wide reconcilers (node
addressing, runtime capability) act through. It must be an admin kubeconfig
for the plane THIS host hosts — k3s writes one at
`/etc/rancher/k3s/k3s.yaml`. Empty means those reconcilers stay off, which is
the honest default: without credentials for the hosted plane there is nothing
they can legitimately act on. The visible symptom of leaving it unset on a
peer-hosted plane is `kubectl logs`/`exec` failing with a 502 while scheduling
and pod execution work fine.


Save = restart (the tunnel server is built once at boot, like every other
listener outpost owns).

Three behaviours worth knowing before you use this:

- **Enabling mints the join token** if the host does not have one, so you
  never end up with a control plane that is on but unjoinable. **Disabling
  preserves it** — deleting it would invalidate every worker as a side
  effect of an off/on toggle, and it is inert while nothing is listening.
- **The bind defaults to `127.0.0.1`**, on the assumption that workers reach
  it over the mesh. `--bind-addr 0.0.0.0` accepts joins directly from the
  network; status output flags that case explicitly.
- **The token is a credential** — this cluster's equivalent of the k3s
  node-token. It is deliberately absent from `outpost status` and from
  `cluster control-plane` status output, and is returned only by the
  reveal/rotate verbs, so reading cluster state never puts it on screen.
  **Rotating invalidates every worker's configuration**; they fail on their
  next reconnect until reconfigured.

Platform support per mode:

| mode | linux | macOS | Windows |
|---|---|---|---|
| `agent` (k3s-agent in a container) | yes | yes (podman VM) | yes (WSL VM) |
| `vk-native` / `vk-ollama` (pods → host processes) | yes | yes | yes |
| `vk-podman` (pods → libpod containers) | yes | yes | yes |

Two things to know before choosing a mode on macOS or Windows:

- **Pods in containers there run inside podman's Linux VM**, so the host
  GPU is not visible to them (Apple Metal can't be passed through, and
  the GPU device plugin only counts `nvidia.com/gpu`). A workload that
  needs the host's accelerator wants `vk-native`, which realizes pods as
  native host processes; a virtual node declares its own capacity, so it
  can advertise Metal as schedulable.
- **On Windows, podman serves its API through `win-sshproxy.exe`**, which
  it locates via containers.conf's `helper_binaries_dir` or
  `$CONTAINERS_HELPER_BINARY_DIR`. With neither, `podman machine start`
  reports success but publishes no endpoint — no pipe, no api.sock — and
  every client sees that as "podman is not installed". `bashy podman`
  provisions the helper and exports the dir, which is why outpost prefers
  `bashy podman` over a PATH podman when it brings the runtime up on
  Windows. A bashy older than that fix leaves the machine unreachable;
  upgrade bashy on the host, or set `$CONTAINERS_HELPER_BINARY_DIR` to a
  directory containing `win-sshproxy.exe` yourself. Outposts only join their
owning cloudbox's cluster — the older paste-a-kubeconfig path
(`outpost_set_kubeconfig`) was removed; cloudbox provides the kubeconfig
automatically once `cluster.enabled` is set.

### Joining a peer-hosted control plane

The worker-side twin of control-plane placement. Empty means "join the
cloudbox-hosted plane", which is the default and the historical behaviour.

| Field | File key | CLI | UI | MCP |
|---|---|---|---|---|
| Peer's tunnel endpoint | `cluster.join_endpoint` | `cluster join <endpoint>` | Cluster > Join a peer's plane | `outpost_cluster_join_peer` |
| Peer's tunnel token | `cluster.join_token` | `cluster join --token` / `--token-stdin` / `$OUTPOST_CLUSTER_JOIN_TOKEN` | `has_join_token` flag only | `outpost_cluster_join_peer` |
| Peer's STCP secret | `cluster.stcp_secret` | `cluster join --stcp-secret` / `$OUTPOST_CLUSTER_STCP_SECRET` | `has_stcp_secret` flag only | `outpost_cluster_join_peer` |
| Peer's k3s node token | `cluster.node_token` | `cluster join --node-token` / `$OUTPOST_CLUSTER_NODE_TOKEN` | `has_node_token` flag only | `outpost_cluster_join_peer` |
| Local apiserver port | `cluster.k8s_api_port` | `cluster join --api-port` | — | `outpost_cluster_join_peer` |
| Agent Node on the joined plane | `cluster.runtimes.agent` | `cluster join --cluster-agent=on/off` | Inbound > Cluster (same field) | `outpost_cluster_join_peer` (`cluster_agent`) |
| Virtual Nodes on the joined plane | `cluster.runtimes.virtual` | `cluster join --cluster-virtual vk-podman,vk-native` | Inbound > Cluster (same field) | `outpost_cluster_join_peer` (`cluster_virtual`) |
| Which plane this host joins | (read-only) | `cluster join --show` | Cluster > Join a peer's plane | `outpost_cluster_peer_plane` |
| Revert to the cloudbox plane | (clears the four above) | `cluster join --clear` | Cluster > Join a peer's plane | `outpost_cluster_leave_peer` |
| Cloudbox's STCP secret (overlay relay) | `cluster.cloud_stcp_secret` | (captured automatically) | `has_cloud_stcp_secret` flag only | — |

Save = restart (cluster runtime config is read once at boot).

Five behaviours worth knowing:

- **A peer join needs FOUR values, not one.** The endpoint plus three
  credentials, all printed by the hosting machine:

  ```bash
  # on the host running the plane
  outpost cluster control-plane token   # endpoint + join token + stcp secret
  outpost cluster token                 # k3s node token

  # on the worker
  outpost cluster join 10.0.0.5:7000 --token T --stcp-secret S --node-token K10…
  ```

  A worker given a subset authenticates and then fails to reach the apiserver
  — a failure that reads like a broken network rather than a missing
  credential, which is why `--show` reports all three presences and warns when
  any is absent.
- **Joining enables cluster mode**, selecting the `agent` runtime when no
  runtime is configured. A join that persisted an endpoint and left the host
  not joining anything would be a config editor wearing a verb's name. An
  existing runtime selection is never overwritten.
- **A peer join can select virtual-kubelet runtimes.** `--cluster-agent` /
  `--cluster-virtual` write the same `cluster.runtimes.agent` and
  `cluster.runtimes.virtual` fields `builtins set` writes, with the same
  partial-update rules: an omitted flag leaves the persisted value alone, and
  `--cluster-virtual` replaces the complete set. A peer-hosted plane supports
  vk nodes as-is — vk authenticates to k3s with client certificates rather
  than a cloudbox-minted bearer token — so `vk-podman` and `vk-native`
  workloads run on one.

  ```bash
  # agent + a vk-podman node on the joined plane
  outpost cluster join 10.0.0.5:7000 --token T --cluster-virtual vk-podman

  # vk only, no k3s agent
  outpost cluster join --cluster-agent=off --cluster-virtual vk-podman,vk-native
  ```

  Naming NEITHER flag is the default and is unchanged: the `agent` runtime,
  and only when nothing is selected yet. Naming either one is an explicit
  choice, so it does change an existing selection — that is the only way a
  join ever rewrites one. Deselecting everything at once is refused rather
  than silently reinstating the agent runtime.
- **This is independent of cloudbox pairing.** The host stays paired for apps,
  shell, the LLM pool and the fleet registry; only cluster membership moves.
  While a peer endpoint is set, the boot reattach deliberately leaves cluster
  credentials alone — cloudbox's node token describes a different cluster.
  The one exception is `cluster.cloud_stcp_secret`: cloudbox's own STCP
  secret, captured automatically (at pairing, boot reattach, and at the join
  itself before the peer's secret overwrites `stcp_secret`) and never typed by
  an operator. The peer-flannel worker's runtime container uses it to open a
  second frpc session directly to cloudbox carrying the `overlay-control`
  visitor — the only path a Tailscale (ts2021) registration can take to
  cloudbox's Headscale, since the public HTTPS URL cannot carry it. Without
  it the agent runtime refuses to start (a node whose flannel is pinned to an
  addressless `tailscale0` would join Ready and silently drop every pod
  packet); `has_cloud_stcp_secret` in the cluster status is the flag to check
  when that refusal appears in the logs.
- **The credentials are write-only.** `cluster.join_token`, `stcp_secret` and
  `node_token` are reported as `has_*` presence flags in `GET /api/config`,
  `outpost status` and every MCP status read, and there is no reveal verb on
  this side: the machine that minted them is the place to read them.
- **`--clear` reverts to the cloudbox-hosted plane** and clears all four
  fields — including the peer-issued node token and STCP secret, which
  describe the peer's cluster. It leaves cluster mode ON; use
  `outpost cluster leave` to stop being a node at all.

To keep secrets out of argv and shell history, every credential has an
environment fallback and the join token can be read from stdin:

```bash
outpost cluster token | ssh worker outpost cluster join 10.0.0.5 --token-stdin
```

`--offline` writes `agent.json` directly for installer scripts that provision
a host before the daemon first starts.

### Applying bundles to a peer-hosted plane (live)

Install an app/bundle manifest set against a **peer-hosted** control plane,
addressed purely by a kubeconfig path — **no cloudbox anywhere on the apply
path** (no `access_token`, no `/api/v1`, no overlay credential fetch). All
surfaces converge on `admincore.BundleApply`; the standalone
`script/dks-peer-bundle-apply.sh` remains the shell-only twin. See
`docs/peer-dks-bundle-apply.md` for the full semantics.

| Field | File key | CLI | UI | MCP |
|---|---|---|---|---|
| Default kubeconfig venue | `cluster.bundle_kubeconfig` | `bundle apply --kubeconfig … --save-kubeconfig` | — | `outpost_apply_bundle` (`kubeconfig` + `save_kubeconfig`) |
| Show the persisted venue | (read-only) | `bundle kubeconfig` | — | `outpost_bundle_kubeconfig` |
| Apply a bundle | (operation, not persisted) | `bundle apply <file-or-dir>` | — | `outpost_apply_bundle` |
| Default OSS appstore catalog | `cluster.bundle_catalog` | `bundle install <name> --catalog … --save-catalog` | — | `outpost_install_builtin` (`catalog` + `save_catalog`) |
| Install an OSS built-in | (operation, not persisted) | `bundle install <name>` | — | `outpost_install_builtin` |
| Check an OSS built-in's live state | (read-only) | `bundle status <name>` | — | `outpost_builtin_status` |
| Remove an OSS built-in | (operation, not persisted) | `bundle uninstall <name>` | — | `outpost_uninstall_builtin` |
| List the effective catalog | (read-only) | `bundle catalog` | — | `outpost_bundle_catalog` |

Save = **live**. `cluster.bundle_kubeconfig` is read on each apply; changing
it never restarts the daemon (the operation touches the peer cluster, not
this daemon's own wiring).

Behaviours worth knowing:

- **Venue resolution order:** explicit `--kubeconfig` / `kubeconfig` arg →
  persisted `cluster.bundle_kubeconfig` → the conventional peer path
  `~/.kube/outpost-control-plane/k3s.yaml`. Whatever the source, the path is
  canonicalized (tilde, relative, `..`, symlinks all resolved) and the
  **cloudbox kubeconfig (`~/.kube/outpost.yaml`) is refused** — a symlinked
  or relative spelling of it included. A path that cannot be canonicalized
  fails; it is never "probably fine".
- **Readiness means rolled out.** The bounded wait (`--timeout`, default
  300 s) requires updated replicas — not merely ready ones — StatefulSet
  `currentRevision == updateRevision`, and `observedGeneration` caught up. A
  workload with `spec.replicas: 0` is a hard failure without the explicit
  `--allow-scale-to-zero` opt-in.
- **Custom resources wait for their CRD.** A CR whose CRD ships in the same
  bundle applies only after the CRD reports `Established` AND the type
  appears in discovery, bounded by `--crd-timeout` (default 60 s); timing
  out is a hard failure.
- **Failures roll back what the run created** — reverse order, bounded,
  best-effort — and report precisely what was and was not cleaned.
  Pre-existing objects are never deleted. `--no-rollback` opts out (still
  reported).
- **`--save-kubeconfig` persists only after a successful apply**, so a saved
  default that never worked can't become a trap for the next run.
- **Built-in names resolve only through the OSS appstore.** `bundle install
  headlamp` resolves exactly `builtin/headlamp/install.yaml` beneath an explicit
  `--catalog`, persisted `cluster.bundle_catalog`, or a sibling `appstore`
  checkout in the umbrella. Canonical-path containment rejects traversal and
  escaping symlinks. Unknown or unavailable assets fail closed; there is no
  cloudbox/private-manifest or network fallback. A catalog can also be a
  fetched, versioned appstore tree. Current compatible names are shown by
  `bundle catalog`; the OSS tree includes `core-storage`, `coredns`, `headlamp`,
  `gpu-device-plugin`, and other appstore built-ins.
- **`--save-catalog` persists only after a successful install** and requires an
  explicit `--catalog`.
- **`bundle status <name>` is read-only** — it applies nothing. It resolves
  the same `builtin/<name>/install.yaml` `bundle install` would apply and
  reports each object's live state (`exists`, `ready`, a short reason) using
  the identical readiness evidence the apply's rolled-out wait uses, so
  `installed`/`all_ready` mean exactly what a successful install would have
  confirmed. `--allow-scale-to-zero` mirrors the apply flag.
- **`bundle uninstall <name>` resolves the same manifest `bundle install`
  applies** and removes every object in reverse apply order — one delete
  failure does not stop the rest, and is reported precisely as an object the
  operator must remove by hand. It reuses the identical deletion mechanics an
  apply failure's own rollback uses, and the same cloudbox-kubeconfig venue
  guard. `--timeout` optionally bounds a wait for the deleted objects to
  actually vanish (finalizers, garbage collection); 0 (default) skips the
  wait.
- `--offline` runs the apply/status/uninstall in the CLI process against the
  on-disk `agent.json` — no running daemon required.

### Networking (boot-time-bound)

| Field | File key | CLI | UI | MCP |
|---|---|---|---|---|
| Matrix-tunnel ingress bind | `local_addr` | `start --addr`, `config set --local-addr`, `$AGENT_ADDR` | Pair tab > Advanced | `outpost_set_networking` |
| VNC upstream for /desktop | `vnc_addr` | `start --vnc-addr`, `config set --vnc-addr`, `$AGENT_VNC_ADDR` | Pair tab > Advanced | `outpost_set_networking` |
| Admin UI + MCP bind | `admin_addr` | `start --admin-addr`, `config set --admin-addr`, `$OUTPOST_ADMIN_ADDR` | Pair tab > Advanced | `outpost_set_networking` |

Defaults:

- `local_addr` → `127.0.0.1:0` (random port)
- `vnc_addr`   → `127.0.0.1:5900`
- `admin_addr` → `127.0.0.1:17777`

Binding `admin_addr` to `0.0.0.0:17777` makes the admin UI / MCP
reachable from the LAN; outpost logs a warning at startup and the
session-cookie gate is enforced on every `/api/*` call (no first-run
bypass).

Use `--admin-addr '<clear>'` (or empty string in the SPA) to revert
a field to its default.

### Intra-home distributed-inference backend (cluster LLM)

| Field | File key | CLI | UI | MCP |
|---|---|---|---|---|
| Cluster backend endpoint | `cluster_llm_endpoint` | `config set --cluster-llm-endpoint URL` | Pair tab > Advanced | `outpost_set_networking` |
| Cluster backend API key | `cluster_llm_api_key` | `config set --cluster-llm-api-key KEY` | Pair tab > Advanced | `outpost_set_networking` |

When this home runs a distributed-inference backend (GPUStack first;
any runtime that publishes the same OpenAI `/v1-openai` surface later)
that tensor/pipeline-splits a single model across several machines, set
`cluster_llm_endpoint` to its base URL (e.g. `http://127.0.0.1:18080`).
The Ollama pool watcher then attaches a cluster descriptor to its
registry push so cloudbox's router can send a model too large for any
one machine to this home. Detection is HTTP-probe only — outpost never
launches the backend (the operator runs it as a container against the
ycode-published podman socket).

`cluster_llm_api_key` is optional: without it the backend is still
detected as running (the admin UI shows it), but the worker/VRAM
aggregation that tells cloudbox "this cluster can hold an N-byte model"
needs the key — GPUStack's management API is Bearer-gated — so the
cloudbox size filter stays inert until a key is supplied. Empty
`cluster_llm_endpoint` (the default) disables detection entirely: the
outpost stays an ordinary single-machine pool member. Both are
boot-time-bound (a change restarts the daemon). The key is redacted
from `SafeView` like other secrets.

### Peer image distribution (recipes, not blobs)

| Field | File key | CLI | UI | MCP | Effect |
|---|---|---|---|---|---|
| Peer image distribution opt-in | `peer_image.enabled` | `builtins set --peer-image` / `agent.json` | Inbound > Built-ins | `outpost_set_builtins` | Restart |
| Mesh service for recipe index | `peer_image.service` | `builtins set --peer-image-service` / `agent.json` | Inbound > Built-ins | `outpost_set_builtins` | Restart |

Peer image verbs:

| Verb | CLI | MCP Tool | Arg(s) | Side-effect class |
|---|---|---|---|---|
| Publish recipe | `outpost peer-image publish` | `outpost_publish_image_recipe` | `name`, `body` | Live |
| Resolve mesh peers | `outpost peer-image mesh-resolve` | `outpost_mesh_resolve_image_recipes` | `service`, `minimum` | Live |
| Ensure image resident | `outpost peer-image ensure` | `outpost_ensure_image` | `name` | Live |
| Challenge report | `outpost peer-image report` | `outpost_report_image` | `node`, `ref`, `recipe_digest`, `nonce` | Live |

### Admin allowlist

| Field | File key | CLI | UI | MCP |
|---|---|---|---|---|
| OS-auth admin emails | `admin_users` (`[]string`) | `config set --admin-users a@x,b@x` / `--clear-admin-users`, `$MATRIX_ADMIN_USERS` | Pair tab > Advanced | `outpost_set_networking` (with `set_admin_users=true`) |

Empty list = legacy behavior (anyone who can prove the OS password
is admin). Non-empty = strict allowlist; non-listed OS users get
`user` role. Ignored when `auth_url` is set (the external endpoint
owns role assignment then).

Normalization on save: trim, lowercase (emails are case-insensitive),
dedup.

### Credentials internal to outpost

These exist for the daemon's own auth surfaces. They are auto-managed
and never need operator input under normal use.

| Field | File key | Rotated via | Purpose |
|---|---|---|---|
| Admin UI session HMAC key | `admin_session_key` (`[]byte`) | (auto on first boot; persists across restarts) | Signs the SPA's session cookies |
| MCP bearer token | `mcp_bearer_token` (hex) | `mcp rotate-token` / `outpost_rotate_mcp_token` / SPA "Rotate" button | Auth for the MCP server at `/mcp/*` |
| File Browser signing key | `files_signing_key` (`[]byte`) | (auto on first use; persists across restarts) | Signs the stateless File Browser embed's session JWTs so sessions survive a daemon restart |

The MCP bearer is shown to the operator (it's what they paste into
`.mcp.json`). The admin-UI session key is never exposed — only the
SPA needs to know it exists.

## Inspecting current state

| Surface | Command |
|---|---|
| CLI table | `outpost status` (paired + builtins + apps + outbound) |
| CLI table | `outpost config show` (networking + admin users) |
| CLI table | `outpost builtins show` |
| CLI JSON | any of the above with `--json` |
| File raw | `cat ~/.config/matrix/agent.json` (mode 0600 — same OS user) |
| MCP resource | `outpost://status` (paired-or-not + agent name) |
| MCP resource | `outpost://config` (full redacted FileConfig) |
| MCP resource | `outpost://apps` |
| MCP resource | `outpost://outbound` |
| UI | the SPA at `http://127.0.0.1:17777` after pairing |

`outpost://config` is the canonical machine-readable snapshot —
identical shape to what the SPA's `/api/config` returns, with every
secret redacted (`token` → `has_token: true/false`, etc.).

## Consistency invariants

By design, all four surfaces converge on a single business-logic
layer (`internal/agent/admincore`). Concretely:

- The admin UI's `POST /api/<x>` and the MCP tool `outpost_<x>` and
  the CLI subcommand `outpost <x>` all call the same admincore method.
- Validation runs once. The CLI can't accept a config the UI would
  reject, and vice versa.
- A change made through one surface is visible on the other two
  immediately — they share the same in-memory `AppRegistry`,
  `OutboundManager`, and (after rotation) `MCPBearerToken`.
- The file is the source of truth; the in-memory mirrors are
  rehydrated from it on every operator-driven save.

If you find a setting where one of those invariants is broken, that
is a bug — file it.
