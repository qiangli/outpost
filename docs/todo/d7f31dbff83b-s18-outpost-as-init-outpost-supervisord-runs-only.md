---
id: d7f31dbff83b
kind: feature
title: 'S18 outpost as init: outpost supervisord runs only the daemon and bashy supervisord; exposure follows unit state; toggles call bashy service; old agent.json service config removed'
seq: 1887
status: blocked
priority: p1
labels:
    - outpost
    - bashy
created: 2026-10-11T05:07:50.314316Z
weave: 3
sprint: 418
sprint_id: 9891800f-8da5-5338-92bc-1d75755ca548
sprint_title: 'Agent ecosystem synergy: agentic tools compose like a|b|c through bashy extension points'
resolution: fixed
closed_by: claude-opus5-c
---

Design docs/bashy-service-design.md: inetd/systemd split - bashy service owns unit definitions and per-unit verbs; bashy supervisord (a bashy command, like inetd) is the service monitor; outpost supervisord (init, like systemd) starts and watches only the outpost daemon and bashy supervisord. Cross-repo (outpost + bashy + yoke). outpost supervisord (OS service via outpost service install) starts and watches exactly the outpost daemon and bashy supervisord, restarting either with backoff; it never watches individual services. Remove the 30s bashy_services poller (superviseBashyService, startBashyService, bashyServiceRunning in cmd/outpost/main.go) and the supervised programs list. Remove agent.json bashy_services, supervised, meet_enabled, loom_enabled and bashy_apps_enabled plus their readers and old lifecycle paths. Define all currently supported services directly as unit YAML; no generic migration, config conversion or old-key compatibility period (operator clarification 2026-10-10 PDT). The daemon registers matrix-tunnel apps (AppRegistry.RegisterWithMeta) and mesh exposures (Forwarder.Expose) from bashy service list --json and the monitor state events: failed or disabled units are withdrawn from tunnel and mesh (today a dead service stays exposed). outpost builtins set, MCP outpost_set_builtins and the admin UI call bashy service enable|disable|set and apply live (today SetBuiltins only returns RestartPending while the CLI claims a restart). Admin UI and MCP show bashy service status --json; unwired internal/agent/apphealth is retired or folded in. Help text distinguishes outpost supervisord (init) from bashy supervisord (service monitor). Acceptance: on a paired host, enabling meet via outpost builtins set exposes /matrix/h/HOST/app/meet/ without an outpost restart; killing bashy supervisord makes outpost restart it and units are re-adopted; a crash-looping unit ends failed and is withdrawn from tunnel and mesh; old agent.json service keys/readers are removed and all currently supported services work from new-format unit definitions; outpost go test -short green.

Operator coverage requirement 2026-10-10 PDT: retain every currently supported service and feature in the new design. Seven initial units are not an exhaustive gate. Account for apps/meet/loom/SDLC/schedule, LLM and DAG workloads, Zot/SeaweedFS/Kopia, act_runner host+sandbox lanes, Headlamp port-forward, custom programs/services and retained in-process outpost surfaces. Preserve supported parameters, provisioning/version pins, environment/secrets, login/elevation/SSO, tunnel/mesh names, controls/status and upgrade behavior. Use the complete service/feature inventory in docs/sprint-418-master-execution-plan.md and shared candidate evidence; every entry needs its new unit or retained owner and existing test/live-smoke evidence. Extend the schema as needed; remove old formats directly with no backward compatibility or migration.

Operator rod-not-fish requirement 2026-10-10 PDT: implement reusable service declaration/configuration/enablement machinery shared by built-in, internal and external services. Built-ins are seeds, not service-name branches. Internal capabilities retain their subsystem behind a generic runtime provider and use the same registry, desired state, health/status/events and exposure contracts; no separate internal feature-flag path or fake child process. Acceptance includes seeded built-in + internal capability + fresh custom external declaration using the same service verbs, plus an operator override, without per-service supervisor/toggle/exposure code. Agree the S15/S16/S18 provider contract first; preserve complete existing feature coverage and no legacy compatibility/migration.

Conductor verify: `go test -short ./...` in outpost on a claimed native host, then paired-host live toggle, re-adoption and failed-unit withdrawal probes. Own outpost init/exposure code. Coordinate the generic status IPC shape with S16 before implementation; preserve every current service and remove retired config directly. Commit with `Agent:` trailer.

Manager review 2026-10-11 07:14 UTC: prior outpost weave #2 committed `54b6fe3` on an unmerged branch, making init start exactly the outpost daemon and bashy monitor and removing the supervisedFromConfig *call*. This is a useful partial, not S18 acceptance. Continue from that commit, then delete `FileConfig.Supervised` and its old keys/readers/tests, convert the existing meet and other supported toggles to generic `bashy service` desired state, consume S16 status IPC for exposure/re-adoption/failed withdrawal, and prove paired-host behavior. Retain all existing services/features. The manager will not merge #2 alone as a complete story.

---

## Outpost-side delivery (weave #3, 2026-10-11)

Branch `agent/weave-issue-3`, continuing from weave #2's `54b6fe3`
(cherry-picked as `bede39e`). yoke pin moved to
`v0.0.0-20261011070059-723d854b025d` for the published S15 contract
(`fleet.Service`, `service.Provider/Status/Event/Internal`).

### Contract agreed with S15/S16

Outpost consumes **S15's types directly**: `service.Status` is the status
shape and `service.State.Exposed()` is the exposure predicate, so no new
wire format was invented. The S16 coupling is one call —
`bashy service status --json`, the monitor's own CLI as the facade over its
status socket, so outpost never needs to know the socket's protocol.

The larger decision: **declarations and desired state are read and written
in process** through fleet's Go API (`svcunit.Store` → `fleet.Catalog`), not
by shelling out. fleet's registry is a library, so
`outpost builtins set --service meet=on` and `bashy service enable meet` are
one write of one sparse overlay through one set of rules (including the
port-conflict check). Only a dotted-path override uses the CLI
(`bashy service set`), because fleet's path editor is unexported. This also
means an internal capability needs no bashy binary at all.

### Done

- **Init**: `outpost supervisord` starts and watches exactly the daemon and
  `bashy supervisord`, with help text naming the three roles (init /
  monitor / systemctl).
- **Removed**, no compatibility reader: `bashy_services`,
  `bashy_apps_enabled`, `bashy_apps_port`, `meet_enabled`, `meet_port`,
  `loom_enabled`, `loom_port`, `supervised`; `conf.BashyService`,
  `DefaultBashyServices`, `SupervisedProgram`,
  `EnsureBashyServiceSSOSecrets`; `effectiveBashyServices`,
  `registerBashyServiceApps`, `startBashyServiceSupervisors`,
  `superviseBashyService`, `startBashyService`, `bashyServiceRunning`;
  `internal/agent/apphealth`.
- **Generic machinery** (`internal/agent/svcunit`): declaration-driven
  exposure reconciler + the internal provider. No code branches on a unit
  name. `svcunit.Capability` is the four-closure adapter a subsystem
  registers through (`cmd/outpost/capabilities.go`).
- **Toggles**: one generic pair (`--service NAME=on|off`,
  `--service-set NAME.PATH=V`) on CLI / MCP `outpost_set_builtins` /
  `POST /api/config/builtins`. Applies live; reports `services_applied` and
  no longer claims an unscheduled restart.
- **Status**: `outpost services`, `GET /api/services`,
  `outpost://services`, `SafeView.services`, and an admin-UI Services
  section rendered from the host's declarations.
- **SSO**: generic `service_sso` map keyed by app route, minted lazily on
  first publication, presence-only on reads.

### Verified

`go test -tags fb_archives -short ./...` → exit 0, 52 packages, 0 failures.
`go vet -tags fb_archives ./...` clean. `GOWORK=off go list -deps ./...` OK
(standalone graph). SPA JavaScript parses (`node --check`). Embedded-docs
drift detector green.

Live probes on a real daemon (scratch XDG dirs, a third-party unit
`openclaw` in the local ring, and a stub supplying S16's status IPC):

| probe | result |
|---|---|
| `builtins set --service openclaw=on` | route 404 → **401** (registered; login required per the declaration), "service exposed on the tunnel", **no restart** |
| `--service openclaw=off` | **404**, "service withdrawn from the tunnel and mesh reason=unit disabled" |
| monitor reports `failed` (start limit) | **404**, reason surfaced verbatim, "reason=unit state failed" |
| monitor returns `active` | **re-adopted**, 401 again |
| monitor stops answering | state `degraded`, route **kept**, `monitor_reachable:false`; on return, re-adopted |
| `--service nope=on` | refused, naming the eight declared units |

Embedded S15 seeds (apps, events, llm, loom, meet, schedule, sdlc) and the
third-party unit all listed with ring provenance; the custom unit needed no
outpost code.

### Not done — blocked on the producers

The installed bashy has **neither** `bashy service` (yoke S15 is merged at
source but not published into a bashy release) **nor** the monitor meaning
of `bashy supervisord` (S16/S19 in flight). So:

1. **Paired-host `/matrix/h/HOST/app/meet/` reachability** is unproven. The
   tunnel registration is proven (401/404 above); what is missing is a
   monitor that actually starts `meet`. Re-run once S16/S17 publish.
2. **`outpost supervisord` launching the real monitor** cannot be smoked:
   today's `bashy supervisord` is still the DAG-root form, so the init child
   exits and backs off. Non-fatal — the daemon is unaffected — and S19
   retires that form.
3. **In-process subsystems** (files, sandbox, podman/ollama proxies, Zot,
   SeaweedFS, Kopia, Headlamp) keep their current boot wiring.
   `outpostCapabilities()` is empty and its contract is tested; moving one
   is a declaration (S17) plus one entry there, with no other outpost
   change. Converting them now would need outpost to ship competing seeds
   for names S17 owns, and would silently disable a working feature on any
   host that has them on.

### Cross-repo follow-up required

`yoke/pkg/webconsole/host_auth.go` reads `bashy_services[].sso_secret` out
of outpost's `agent.json` for its trusted-header check. That key is gone;
it must read `service_sso[<app route>]` instead. Until it does, a bashy
webconsole unit published with `trust_cloud_identity` will not find its key.
