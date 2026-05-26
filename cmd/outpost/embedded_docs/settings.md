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
| Agent name | `agent_name` | `register --name` (alias: `outpost pair`), `start --name` | Pair tab | `outpost_pair` | Restart |
| Portal server | `server_addr` / `server_port` / `protocol` | `register --server`, `start --server / --server-port`, `$MATRIX_SERVER_ADDR`, `$MATRIX_SERVER_PORT`, `$MATRIX_PROTOCOL` | Pair tab (display only) | `outpost_pair` | Restart |
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
| Podman daemon proxy | `podman_enabled` | `builtins set --podman` | Inbound > Built-ins | `outpost_set_builtins` | Restart |
| Ollama daemon proxy | `ollama_enabled` | `builtins set --ollama` | Inbound > Built-ins | `outpost_set_builtins` | Restart |
| Ollama LLM-pool participation | `ollama_pool_enabled` | `builtins set --ollama-pool` | Inbound > Built-ins | `outpost_set_builtins` | Restart |
| Cluster join | `cluster.enabled` | `builtins set --cluster` or `cluster set --enable` | Inbound > Cluster | `outpost_set_builtins` / `outpost_set_kubeconfig` | Restart |
| Cloudbox-pushed self-upgrade | `auto_upgrade` | `builtins set --auto-upgrade` | Inbound > Built-ins | `outpost_set_builtins` | Live |

All built-in toggles default to ON when the JSON key is absent (old
configs) so an upgrade doesn't silently disable features. The
exceptions are `podman_enabled` / `ollama_enabled` which are plain
`bool` (default off — explicit opt-in).

`auto_upgrade` is the only built-in toggle with **Live** effect — the
upgrade worker re-reads the FileConfig on each `POST /admin/upgrade`,
so flipping it doesn't require (and doesn't trigger) a restart. Default
is **on** for paired hosts; flip off via `outpost builtins set
--auto-upgrade=off` to freeze a specific box on its current build (e.g.
during a debugging session you don't want a cloudbox release to
disturb). Unpaired hosts ignore the flag — the `/admin/upgrade` route
only mounts once cloudbox has issued an `access_token`.

#### Cloudbox-pushed upgrade flow

When `auto_upgrade` is on, cloudbox POSTs to `<this-host>/admin/upgrade`
through the matrix tunnel with `Authorization: Bearer <access_token>`
and an envelope shaped like:

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

Status codes the daemon returns to cloudbox:

| HTTP | Status | Meaning |
|---|---|---|
| 202 | accepted | upgrade staged + worker goroutine running |
| 200 | replay | same `release_id` already handled this run (idempotent) |
| 409 | in_flight | another upgrade is currently running |
| 304 | same_commit | daemon is already at this commit |
| 403 | disabled | operator turned `auto_upgrade` off |
| 412 | min_from | daemon's current commit is older than `min_from` requires |
| 400 | (invalid envelope) | required field missing or `url` is not https |
| 401 | (auth) | bearer token missing or wrong |

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
| Enabled | `enabled` | `--disabled` inverts | Toggle |
| Require login | `require_login` | `--require-login` | Checkbox |
| LAN-only paths | `lan_only_paths` | `--lan-only-path /p` (repeatable) | Textarea |
| Index path | `index_path` | `--index-path` | Index path |
| Trust cloud identity | `trust_cloud_identity` | `--trust-cloud-identity` | Checkbox |
| Provisioning token | `provisioning_token` | auto-generated; rotate with `apps rotate-token` | Reveal / Copy / Rotate |

MCP equivalents: `outpost_upsert_app`, `outpost_delete_app`,
`outpost_rotate_app_token`, `outpost_suggest_apps`.

App add / update is **live** — the running `AppRegistry` is mutated
under a mutex, no restart needed. App removal is also live.

### Outbound mounts (live)

`outbound[]` is a slice of `OutboundConfig`. Each entry:

| Field | File key | CLI flag on `outbound add` | UI input |
|---|---|---|---|
| Local path / identifier | `path` | `--path` | Path |
| Remote app name | `name` | `--name` | (auto from dropdown) |
| Remote host | `host` | `--host` | (auto from dropdown) |
| Remote OS user | `user` | `--user` | (auto from dropdown) |
| Scheme | `scheme` | `--scheme` (`http`, `tcp`, `ssh`) | Scheme |
| Local port | `local_port` | `--port` | Port (tcp/ssh only) |
| TTL override | `ttl_seconds` | `--ttl` | TTL selector |

MCP equivalents: `outpost_upsert_outbound`, `outpost_delete_outbound`,
`outpost_connect_outbound`, `outpost_disconnect_outbound`,
`outpost_suggest_outbound`.

Add / remove / connect / disconnect are all **live**. `connect`
requires the user's OS password on the remote host (human-in-the-loop
for agent calls).

### Cluster (virtual-podman)

| Field | File key | CLI | UI | MCP |
|---|---|---|---|---|
| Joined | `cluster.enabled` | `cluster set --enable` / `cluster clear` | Inbound > Cluster | `outpost_set_kubeconfig` / `outpost_clear_kubeconfig` |
| Apiserver URL | `cluster.api_url` | (parsed from kubeconfig YAML) | display | (parsed from kubeconfig) |
| Bearer token | `cluster.token` | (parsed from kubeconfig YAML) | `has_token` flag only | (parsed; never read back) |
| CA bundle | `cluster.ca` | (parsed from kubeconfig YAML) | `has_ca` flag only | (parsed; never read back) |
| Node name override | `cluster.node_name` | `cluster set --node-name` | Inbound > Cluster | `outpost_set_kubeconfig` |

Save = restart (the vkpodman runner is built once at boot).

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
