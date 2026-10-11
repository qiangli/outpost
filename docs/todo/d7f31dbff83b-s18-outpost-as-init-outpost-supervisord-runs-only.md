---
id: d7f31dbff83b
kind: feature
title: 'S18 outpost as init: outpost supervisord runs only the daemon and bashy supervisord; exposure follows unit state; toggles call bashy service; old agent.json service config removed'
seq: 1887
status: assigned
priority: p1
labels:
    - outpost
    - bashy
created: 2026-10-11T05:07:50.314316Z
weave: 2
assignee: codex-gpt5.6-terra
sprint: 418
sprint_id: 9891800f-8da5-5338-92bc-1d75755ca548
sprint_title: 'Agent ecosystem synergy: agentic tools compose like a|b|c through bashy extension points'
---

Design docs/bashy-service-design.md: inetd/systemd split - bashy service owns unit definitions and per-unit verbs; bashy supervisord (a bashy command, like inetd) is the service monitor; outpost supervisord (init, like systemd) starts and watches only the outpost daemon and bashy supervisord. Cross-repo (outpost + bashy + yoke). outpost supervisord (OS service via outpost service install) starts and watches exactly the outpost daemon and bashy supervisord, restarting either with backoff; it never watches individual services. Remove the 30s bashy_services poller (superviseBashyService, startBashyService, bashyServiceRunning in cmd/outpost/main.go) and the supervised programs list. Remove agent.json bashy_services, supervised, meet_enabled, loom_enabled and bashy_apps_enabled plus their readers and old lifecycle paths. Define all currently supported services directly as unit YAML; no generic migration, config conversion or old-key compatibility period (operator clarification 2026-10-10 PDT). The daemon registers matrix-tunnel apps (AppRegistry.RegisterWithMeta) and mesh exposures (Forwarder.Expose) from bashy service list --json and the monitor state events: failed or disabled units are withdrawn from tunnel and mesh (today a dead service stays exposed). outpost builtins set, MCP outpost_set_builtins and the admin UI call bashy service enable|disable|set and apply live (today SetBuiltins only returns RestartPending while the CLI claims a restart). Admin UI and MCP show bashy service status --json; unwired internal/agent/apphealth is retired or folded in. Help text distinguishes outpost supervisord (init) from bashy supervisord (service monitor). Acceptance: on a paired host, enabling meet via outpost builtins set exposes /matrix/h/HOST/app/meet/ without an outpost restart; killing bashy supervisord makes outpost restart it and units are re-adopted; a crash-looping unit ends failed and is withdrawn from tunnel and mesh; old agent.json service keys/readers are removed and all currently supported services work from new-format unit definitions; outpost go test -short green.

Operator coverage requirement 2026-10-10 PDT: retain every currently supported service and feature in the new design. Seven initial units are not an exhaustive gate. Account for apps/meet/loom/SDLC/schedule, LLM and DAG workloads, Zot/SeaweedFS/Kopia, act_runner host+sandbox lanes, Headlamp port-forward, custom programs/services and retained in-process outpost surfaces. Preserve supported parameters, provisioning/version pins, environment/secrets, login/elevation/SSO, tunnel/mesh names, controls/status and upgrade behavior. Use the complete service/feature inventory in docs/sprint-418-master-execution-plan.md and shared candidate evidence; every entry needs its new unit or retained owner and existing test/live-smoke evidence. Extend the schema as needed; remove old formats directly with no backward compatibility or migration.

Operator rod-not-fish requirement 2026-10-10 PDT: implement reusable service declaration/configuration/enablement machinery shared by built-in, internal and external services. Built-ins are seeds, not service-name branches. Internal capabilities retain their subsystem behind a generic runtime provider and use the same registry, desired state, health/status/events and exposure contracts; no separate internal feature-flag path or fake child process. Acceptance includes seeded built-in + internal capability + fresh custom external declaration using the same service verbs, plus an operator override, without per-service supervisor/toggle/exposure code. Agree the S15/S16/S18 provider contract first; preserve complete existing feature coverage and no legacy compatibility/migration.

Conductor verify: `go test -short ./...` in outpost on a claimed native host, then paired-host live toggle, re-adoption and failed-unit withdrawal probes. Own outpost init/exposure code. Coordinate the generic status IPC shape with S16 before implementation; preserve every current service and remove retired config directly. Commit with `Agent:` trailer.

Manager review 2026-10-11 07:14 UTC: prior outpost weave #2 committed `54b6fe3` on an unmerged branch, making init start exactly the outpost daemon and bashy monitor and removing the supervisedFromConfig *call*. This is a useful partial, not S18 acceptance. Continue from that commit, then delete `FileConfig.Supervised` and its old keys/readers/tests, convert the existing meet and other supported toggles to generic `bashy service` desired state, consume S16 status IPC for exposure/re-adoption/failed withdrawal, and prove paired-host behavior. Retain all existing services/features. The manager will not merge #2 alone as a complete story.
