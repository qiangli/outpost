---
id: aec2bc6b0cf1
kind: feature
title: S18 bind retained outpost in-process surfaces to internal service units
seq: 1889
status: todo
priority: p1
labels:
    - outpost
    - service
created: 2026-10-11T09:12:43.123921Z
weave: 5
sprint: 418
sprint_id: 9891800f-8da5-5338-92bc-1d75755ca548
sprint_title: 'Agent ecosystem synergy: agentic tools compose like a|b|c through bashy extension points'
---

Scoped child of S18 d7f31dbff83b after external ownership child d24c64abdb42. In-process outpost handlers retain their owning subsystems but must use the SAME service registry declaration/config/enablement/status/exposure machinery as built-in and external units. Bind internal capabilities outpost.files (File Browser allow_write/scope and login), outpost.podman (admin role/socket detection), outpost.sandbox (memory/cpus/pids/max containers/images/scratch/prewarm), outpost.ollama and outpost.ollama-pool, outpost.warm, outpost.shard, outpost.lan-inference, outpost.otel and outpost.ycode. Preserve all existing behavior, supported settings, proxy/intercept paths, port conflicts, secrets/security/identity, pool/warm/shard health, and mesh/app exposure. Remove old parallel enablement/config keys and direct lifecycle paths without compatibility/migration; map admin/MCP toggles onto bashy service enable|disable|set and status. Use the shared internal provider contract from S15; no service-name conditionals in the generic reconciler. Source of exact inventory: umbrella docs/bashy-service-design.md Retained service and feature coverage table. Start after external S18 candidate merges to avoid cmd/outpost/main.go overlap. Worker verify GOWORK=off go test -short ./cmd/outpost/... ./internal/agent/conf/... ./internal/agent/svcunit/... ./internal/agent/admincore/... plus relevant sandbox/ollama/warm/shard tests; no skipped legacy regression without a replacement assertion. Manager independently runs Linux full outpost gate and paired-host live toggles. Original S18 remains open until complete coverage and exposure verification.

## Delivery (2026-10-11)

All ten capabilities bound plus `outpost.actrunner`: `cmd/outpost/capability_{files,podman,ollama,warm,shard,laninference,otel}.go` and the shared readers in `capability_support.go`, registered in `outpostCapabilities()`. Each registers its own app routes / mesh exposure / proxy wrap / intercept on Activate and withdraws on Deactivate, refuses activation with the reason when its backend is absent, and re-probes in Health so a vanished daemon withdraws the routes.

Retired outright (no reader, no alias): `agent.json` `files_*` (bar the daemon-internal `files_signing_key`), `podman_enabled`, `sandbox_*`, `ollama_enabled`, `ollama_pool_enabled`, `warm_serving_enabled`, `warm_budget_frac`, `lan_inference_*`, `otel_*`, `ycode_*`, the whole `shard` block; `conf.ShardConfig` and the fifteen `*On()` helpers incl. `MeshNeeded`/`PeerPlaneNeeded`; `otel.YcodeSurfaceEnabled`; the matching admincore params, SafeView rows, MCP args, CLI flags and SPA toggles. `admincore.BuiltinView` lost `Enabled` — it reports detection only. Toggles now go through `services` / `service_settings` (the `bashy service enable|disable|set` write) and `outpost services` for status.

Two daemon decisions still read declared desired state because the mesh and peer plane are core functions, not units: `capabilityEnabled(store, capShard)` gates both, replacing the `ShardOn()` cascade. `POST /admin/warm` resolves the executor per request and answers 503-with-reason while the unit is down (was unmounted, i.e. 404).

Replacement assertions for every deleted regression: `cmd/outpost/capabilities_test.go` (capability coverage, the capability-id keying, files publish/withdraw + declared port, sandbox policy + prewarm fallback, podman refusal, ollama mount + health + pool identity binding + intercept non-stacking, LAN port vs reserved ports, warm refusals + applied budget, shard refusal, otel/ycode publish + withdraw + share set, setting readers); `internal/agent/warm/route_test.go` (per-request executor, 503 vs 404); `admincore/builtins_test.go` TestSetBuiltinsRejectsRetiredServiceParams; `adminui/builtins_test.go` TestBuiltinsPodmanOllamaAreServiceUnits + TestBuiltinsConfigureServiceUnitREST; `conf/file_test.go` TestRetiredExternalServiceKeysAreNotRead extended to all of them; `portal/exchange_test.go` asserts a fresh pairing writes no service key.

Verify: `GOWORK=off go test -short -tags fb_archives ./...` green (the two load-flaky tests — runtime TestFRPCSupervisorRestartsAliveProcessWhenVisitorDisappears and shard TestStart_WiresLaunchesAndStops — fail identically on base cba5e68 under full-suite load and pass in isolation). linux/amd64 and windows/amd64 cross-builds pass. Embedded docs re-synced.

Open: the unit DECLARATIONS are yoke S17's (outpost ships no unit YAML), so until the seeds name these capability ids nothing activates — same gap the external-ownership child left. Paired-host live enable/withdraw and the Linux full gate remain the manager's.

Manager integration 2026-10-11 10:47 UTC: source `e3788fb` plus manager removal of duplicate actrunner alias `bca5b33` merged as Outpost `59436c4` and pushed. Linux full suite with paired Bashy binary passed on `e3788fb`; the exact corrected `59436c4` gate and paired live enable/withdraw remain pending. Yoke internal seeds `1d6388b` now exist. The story remains open for exact candidate and paired gate review.
