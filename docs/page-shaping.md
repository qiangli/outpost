# Outpost live-page shaping (Sprint 641)

The admin page registers named handlers with the server's page-control bridge.
The CLI addresses an already-open page; it does not generate HTML, execute
arbitrary scripts, or click DOM selectors. Domain writes remain in `admincore`.

```sh
outpost page list
outpost page invoke PAGE outpost.navigate '"pair"'
outpost page invoke PAGE outpost.networking.get
outpost page invoke PAGE outpost.networking.draft '{"local_addr":"127.0.0.1:18080"}'
outpost page invoke PAGE outpost.networking.submit
outpost page shape-only PAGE on
outpost page shape-only PAGE off
outpost page invoke PAGE outpost.shape.open true
outpost page result COMMAND
```

`page list` reports current page IDs and mounted actions. IDs change on reconnect.
`invoke` sends exactly one command, then polls its result. Disconnect/restart can
leave a mutation's outcome unknown: inspect `config show` before deciding what
to do. Commands are never replayed automatically. Networking may restart the
server. Empty field strings mean restore defaults when submitted.

## Covered controls and parity

| Page operation | Named action / CLI | Domain path |
|---|---|---|
| Five top-level tabs | `outpost.navigate` with pair/inbound/outbound/update/backup | Presentation only |
| Show/close Shape dialog | `outpost.shape.open` boolean (null toggles) | Presentation only |
| Only Shape / restore controls | `page shape-only PAGE on/off/toggle` (`outpost.shape-only`) | Presentation only |
| Inspect networking draft | `outpost.networking.get` | Draft only; persisted equivalent `outpost config show` |
| Edit three networking fields | `outpost.networking.draft` | Draft only; all keys validated before changing any field |
| Save networking | `outpost.networking.submit` | Same function as Save/Enter; existing POST `api/config/networking` → admincore; headless equivalent `outpost config set --local-addr/--vnc-addr/--admin-addr` |

All action invocations use `outpost page invoke PAGE ACTION [JSON]`. Equivalent
HTTP endpoints are GET `api/page/pages`, POST `api/page/invoke` with
`{page_id,action,input}`, and GET `api/page/result?id=COMMAND`.

Shape-only is temporary per-page presentation, not a persisted configuration.
It hides ordinary controls, makes their region inert, and sets input/textarea
readonly while preserving original readonly values for restoration. A separate
Shape entry remains reachable. The dialog offers navigation and restoration.
Login/fatal views remove page registrations and restore ordinary controls.
There is an OS dictation hint; this slice adds no microphone or transcription
service and does not claim browser voice support detection.

## Authority and lifecycle

The normal OS-user session gate applies. Existing local MCP bearer authentication
is accepted only by the existing direct-loopback policy; remote listeners still
require an admin session (`page --cookie-file FILE` may select it). Page IDs and
results are partitioned by authenticated OS user. Six named actions are explicitly
allowlisted by the server. Credential reveal/rotation, admin-user policy, pairing,
and session operations are excluded, so a bearer cannot borrow a browser's broader
capabilities. Networking writes already permit this same bearer authority.

Logout closes that user's page sockets and invalidates pending commands. Socket
credentials are revalidated every second; the shared runtime also bounds socket
lifetime. Shutdown closes the bridge. No capabilities outlive a page reconnect.

Scripts and sockets resolve relative to the forwarded base prefix, including
`/host/`. Origin must match Host. A proxy which preserves the public Host works;
a proxy rewriting Host needs a separately authenticated proxy contract. Arbitrary
`X-Forwarded-Host` is deliberately not trusted for this bridge.

## Refactoring boundary

This is the first Outpost slice, not migration of every legacy form. Existing
headless families remain available: config, apps, builtins, services, outbound,
cluster, restart, unpair, upgrade/rollback, bundle, and token/MCP commands. They
continue to use their current authorization and domain paths. No generic action
exposes those legacy controls. Their full field-by-field UI parity has not been
re-audited in this slice; no new UI-only operation was introduced.

Verification: focused Go adapter/CLI tests cover auth, origin, prefix assets,
cookie-page dispatch by an authorized bearer, logout/pending invalidation, single
invoke, and input rejection. `node script/test/page-control.cjs` executes the
shipped presentation and networking handlers with a minimal DOM fixture to check
readonly restoration, draft validation, shared submit, duplicate-submit rejection,
and cleanup. Shared pagecontrol tests cover protocol expiry/replay semantics.

`node script/test/page-control-browser.cjs` runs the shipped handlers and CSS in
headless Chrome with a real DOM, checking field changes, hidden controls, dialog
open/close, readonly restoration, shared human/agent saves, and login cleanup.
It accepts optional Chrome and shared runtime JS paths; defaults use installed
macOS Chrome and the sibling yoke checkout. Network/protocol authorization is
covered separately by the Go websocket adapter test.
