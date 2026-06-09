# Driving outpost from agent tools (MCP)

Outpost exposes its full configuration surface — pair, apps, outbound
mounts, built-in toggles, cluster — to MCP-capable agent tools (Claude
Code, Windsurf, Cursor, ...) as a local HTTP server. Anything a human
can click in the admin UI, an agent can call as a tool.

## Where it lives

The MCP server is mounted on the same loopback listener as the admin
UI (default `127.0.0.1:17777`), at the `/mcp/*` path prefix. One
process, one port, one URL to copy. Auth is a bearer token persisted
in `~/.config/matrix/agent.json` (mode 0600) under the
`mcp_bearer_token` field — distinct from the session cookie the SPA
uses, so the two surfaces never share credentials.

```
outpost daemon
└── 127.0.0.1:17777
    ├── /             SPA (humans, session cookies)
    ├── /api/*        admin REST API (humans)
    └── /mcp/*        MCP streamable HTTP (agent tools, bearer token)
```

## Setup

1. Start outpost once so the token is generated:

   ```
   outpost start
   ```

   The daemon prints

   ```
   Admin UI: http://127.0.0.1:17777
   MCP endpoint: http://127.0.0.1:17777/mcp/  (bearer: <token>)
   ```

   The token is the value of `mcp_bearer_token` in your `agent.json`.
   You can also retrieve it later from the admin UI's "Pair host" tab,
   under "Agent tool credentials (MCP)" — Reveal / Copy / Rotate.

2. Add the entry to your agent's `.mcp.json` (Claude Code shape):

   ```json
   {
     "mcpServers": {
       "outpost": {
         "type": "http",
         "url": "http://127.0.0.1:17777/mcp/",
         "headers": {
           "Authorization": "Bearer <paste-token-here>"
         }
       }
     }
   }
   ```

3. Restart the agent tool so it picks up the config. The `outpost_*`
   tools should appear in its tool list.

## Tool catalog

Every tool name is `outpost_<verb>_<noun>`. Mutation tools return
`{ok, restart_pending?}`; when `restart_pending=true` the daemon
re-execs to bring the change live — poll the `outpost://status`
resource until `configured` settles.

### Pairing

- `outpost_pair` — exchange a one-time portal code for an `agent.json`
  identity. Args: `server`, `code`, `name`, optional `title`,
  `auth_url`, `client_only`.
- `outpost_unpair` — wipe AgentName/Token/AccessToken; keep apps,
  outbound mounts, built-in toggles.

### Apps (inbound custom proxies)

- `outpost_list_apps` — same payload as resource `outpost://apps`.
- `outpost_upsert_app` — add or update by name; pass either `url` or
  the `{scheme, host, port, socket}` quartet. Live mutation, no
  restart.
- `outpost_delete_app` — remove by name (idempotent).
- `outpost_rotate_app_token` — rotate the per-app provisioning bearer
  (requires `trust_cloud_identity=true`).
- `outpost_suggest_apps` — probe well-known sockets and the ycode
  manifest; returns candidates to feed into `outpost_upsert_app`.

### Outbound mounts (to peer outposts)

- `outpost_list_outbound` — same payload as resource
  `outpost://outbound`.
- `outpost_upsert_outbound` — add or update by path. Schemes:
  - `http` (default) — admin-UI subpath
  - `tcp` — local listener on `local_port`
  - `ssh` — local listener bridging to the remote `/ssh` built-in
- `outpost_delete_outbound` — remove by path.
- `outpost_connect_outbound` — clear the cloudbox `/matrix/h/<host>/elev/*`
  gate (`elev/app/<name>` for http/tcp, `elev/ssh` for ssh scheme).
  Takes a `password` arg. **Human-in-the-loop**: agents must ask the
  user for the OS password on every call; do not cache.
- `outpost_disconnect_outbound` — drop the matrix_elev cookie.
- `outpost_suggest_outbound` — fetch the catalog of (host, app) pairs
  the caller can mount, from cloudbox's `/api/v1/hosts`.

### Built-ins

- `outpost_set_builtins` — partial-update toggle for `shell`, `desktop`,
  `clipboard`, `ssh`, `ssh_allow_local_forward`,
  `ssh_allow_remote_forward`, `ssh_allow_agent_forward`,
  `ssh_forward_sockets`, `sftp`, `podman`, `ollama`, `ollama_pool`,
  `cluster`, `otel`, `otel_pool`, `ycode_share`,
  `ycode_share_require_login`, `ycode_share_surfaces`, `update_mode`.
  Only fields actually present are mutated.

### Cluster

- `outpost_clear_kubeconfig` — leave the cluster. Joining is done via
  `outpost_set_builtins` with `cluster: true` once paired; the daemon
  auto-fetches a kubeconfig from cloudbox on next boot. (The earlier
  paste-a-kubeconfig path `outpost_set_kubeconfig` was retired —
  outposts only join their owning cloudbox's cluster.)

### Lifecycle

- `outpost_restart` — trigger a daemon self-restart.
- `outpost_rotate_mcp_token` — mint a fresh MCP bearer; the old token
  stops authenticating IMMEDIATELY. Surface the new value to the
  operator so they can update their `.mcp.json`.

## Resources (read-only)

- `outpost://status` — paired-yet, agent name, server URL, current OS user.
- `outpost://config` — full FileConfig view with secrets stripped
  (`Token`, `AccessToken`, `ProvisioningToken`, `Cluster.Token`,
  `Cluster.CA` never leave the daemon).
- `outpost://apps` — registered custom apps.
- `outpost://outbound` — outbound mounts + live state.

## CLI mirror

For headless / scripted use, the same operations are reachable as
cobra subcommands that call MCP under the hood:

```
outpost apps {list, add, rm, start, stop, secret, rotate-secret, rotate-token, suggest}
outpost builtins {show, set --shell=on/off ...}
outpost status
outpost unpair --yes
```

`outpost apps add` and `outpost apps rm` accept `--offline`, which
bypasses MCP and writes the `FileConfig` directly via a one-shot
`admincore.Server`. Useful for installer scripts that need to
provision a host before the daemon is started for the first time:

```
outpost register --code <code> &&
  outpost apps add ycode --url http://127.0.0.1:8765 --require-login --offline &&
  outpost start
```

## Token rotation

If a token leaks (committed to a repo, pasted into a screenshot,
etc.), rotate it immediately:

- **Admin UI**: Pair tab → Agent tool credentials → Rotate.
- **MCP itself**: call `outpost_rotate_mcp_token`. The new token comes
  back in the response; update your `.mcp.json` before the next call.

The OLD token stops authenticating instantly. The admin UI's session
cookie is unaffected.

## Security posture

- Listener is loopback-only by default. `$OUTPOST_ADMIN_ADDR` can bind
  non-loopback; outpost warns at startup and the bearer-token gate is
  still enforced — there's no first-run bypass like the admin UI has.
- Token is 32 random bytes encoded as hex (64 chars). Constant-time
  comparison; no timing oracle.
- Token rotation is in-memory atomic — no window where both tokens
  authenticate.
- The MCP server **does not have a separate auth scheme from the
  admin UI by accident**: the two middlewares are mounted on disjoint
  route groups (`/api/*` vs `/mcp/*`), so a session cookie never
  authenticates an MCP call and vice versa.

## Phase 2 backlog (not yet implemented)

Listed in the plan file the implementation was driven from:

- `outpost_ensure_app` / `outpost_ensure_outbound` — idempotent desired-state
  helpers for agents that reason about goals rather than transitions.
- `outpost_diagnose_app` / `outpost_diagnose_outbound` / `outpost_diagnose_tunnel` —
  probe connectivity, surface what the logs would say.
- `outpost_describe` — first-contact tool returning version, OS, paired
  status, tool catalog.
- `outpost_recent_logs` — structured slog tail.
- `outpost_validate_config` — dry-run validator without mutation.
- `outpost_apply_config` — bulk atomic apply of a config snapshot.
- Read-only MCP mode (`--mcp-read-only`) for unprivileged agents.

Tracked under the same plan that produced Phase 1.
