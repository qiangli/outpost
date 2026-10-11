# S18 retained service coverage audit

Audit of the retained surfaces named by the S18 continuity record, against
this outpost checkout. The umbrella `docs/bashy-service-design.md` and
`docs/sprint-418-master-execution-plan.md` are not present in this checkout;
the local source of retained-service details is
[`settings.md`](settings.md#bashy-service-units).

| Surface | New owner / declaration | Outpost source and evidence |
|---|---|---|
| Zot registry | `provider: process` unit seed; monitor owns process, outpost owns declared publication | `cmd/outpost/capabilities.go` documents this boundary; generic process publication and withdrawal: `internal/agent/svcunit/svcunit_test.go` (`TestActiveUnitIsPublishedOnTunnelAndMesh`, `TestFailedUnitIsWithdrawnFromTunnelAndMesh`). Seed YAML belongs to yoke/fleet and is not in this checkout. |
| SeaweedFS S3 gateway | `provider: process` unit seed | Same generic process publication/withdrawal tests; seed YAML belongs to yoke/fleet. |
| Kopia backup repo server | `provider: process` unit seed | Same generic process publication/withdrawal tests; seed YAML belongs to yoke/fleet. |
| act_runner host and sandbox lanes | `provider: internal`, capability `outpost.actrunner`; declaration supplies lane, image, version, labels, state path and secrets; outpost resolves forge and host Docker socket | `cmd/outpost/capability_actrunner.go`; `cmd/outpost/services_test.go` (`TestActrunnerCapabilityResolvesInstanceFromTheForgeUnit`) checks settings, vault token precedence, host default, sandbox lane and version. Runtime/provisioning implementation is kept by outpost because registration is a prerequisite and Docker host is host-specific. |
| Headlamp port forward | `provider: process` unit seed; Headlamp installation remains the appstore bundle, forwarding is a unit | `cmd/outpost/embedded_docs/settings.md` (“Headlamp operating UI”); generic process publication/withdrawal tests above. Seed YAML belongs to yoke/fleet. |
| Custom process/service | Operator declaration, `bashy service add`, monitor-owned process | `internal/agent/svcunit/store_test.go` (`TestFreshCustomDeclarationIsEnabledAndExposedLikeABuiltIn`) exercises fresh declaration, enablement and app/mesh publication; `svcunit_test.go` covers failed withdrawal. |
| Internal outpost capabilities | `provider: internal` with `outpost.*` capability ID, registered through `outpostCapabilities` and `svcunit.Hosted`; no synthetic child process | `cmd/outpost/capabilities.go`, `internal/agent/svcunit/capability.go`; `svcunit_test.go` (`TestInternalCapabilityUsesTheSameMachinery`, `TestInternalActivationFailureWithdrawsExposure`); `owner_test.go` (`TestInternalUnitFollowsMonitorDesiredStateAndReports`) covers desired state, report, monitor restart and re-adoption. |
| Retained in-process inventory | `outpost.files`, `outpost.podman`, `outpost.sandbox`, `outpost.ollama`, `outpost.ollama-pool`, `outpost.warm`, `outpost.shard`, `outpost.lan-inference`, `outpost.otel`, `outpost.ycode` | Registration is centralized in `cmd/outpost/capabilities.go`; feature-specific lifecycle and settings tests are in `cmd/outpost/capabilities_test.go` and `cmd/outpost/services_test.go`. |

The shared process-unit tests establish that declarations—not service-name
branches—drive ports, login metadata, app and mesh names, and failed-state
withdrawal. The owner socket tests establish the corresponding desired-state,
health/report, and re-adoption contract for in-process providers. This audit
found no concrete missing outpost coverage path to change. It does not attest
the yoke/fleet seed contents or replace paired-host smoke evidence, which are
outside this checkout and remain manager gates.
