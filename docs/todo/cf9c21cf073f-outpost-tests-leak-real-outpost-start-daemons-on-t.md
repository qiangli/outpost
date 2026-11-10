---
id: cf9c21cf073f
kind: bug
title: outpost tests leak real 'outpost start' daemons on the dev box
seq: 2765
status: done
priority: p1
labels:
    - test
    - hygiene
created: 2026-11-10T15:15:35.196482Z
assignee: codex-gpt6-astra
sprint: 606
sprint_id: ebee3343-b774-520a-9d53-b57b306f4210
sprint_title: Streamline Cloudbox master, satellite, and outpost relationships
closed: 2026-11-10T18:36:23.518064Z
closed_by: claude-opus5.5-s606
---

On the dev box many processes '<tmp>/outpost-e2e-bin-*/outpost start' were running (some 20 days old, several from 2026-11-10 worker test runs). Tests that build and start a real outpost daemon must stop it (t.Cleanup kill of the process group + wait) even on failure/timeout. Find every such test (grep outpost-e2e-bin), fix cleanup, add a guard test. Also reap the leaked ones on this host: only processes whose executable lives under a temp dir named outpost-e2e-bin-*; never touch the real outpost or bashy services.

Delivery evidence (2026-11-10):

- Audited the sole `outpost-e2e-bin-` builder and both daemon launch sites in
  `cmd/outpost/e2e_test.go`; LAN SSH E2E tests also inherit the shared helper.
- Both launch sites now create a private process group, register cleanup before
  readiness checks, kill the group, and wait exactly once. A watchdog stops the
  group before Go's hard test timeout (which does not run `t.Cleanup`). Supervised
  mode prevents daemon restarts from detaching outside the owned group.
- Readiness HTTP requests are bounded; output buffers and exit observations are
  synchronized. Binary building is lazy, and TestMain removes its directory
  before `os.Exit` rather than relying on an unreachable defer.
- Targeted gate passed: `go test -race -tags fb_archives ./cmd/outpost -run
  '^TestDaemonProcessCleanup$' -count=1 -timeout=60s -v`. All four cases passed:
  normal cleanup, repeated explicit stop, fatal failure, and hard timeout.
  Guards use harmless shell/sleep stand-ins, including a TERM-ignoring child.
  No real-daemon E2E, Podman, or VM tests were run.
- Reaped 53 orphaned test daemons on dragon. Verified 50 executable paths with
  macOS `proc_pidpath`; verified the three deleted executable mappings with
  `lsof -d txt`. Signaled only individual PIDs whose executable was `outpost`
  directly inside an `outpost-e2e-bin-*` directory under the host temp directory
  or `/tmp`. Final process scan found zero matching executables. Real outpost
  and bashy services were not targeted.

Service-manager follow-up (2026-11-10, review steering):

- Read bashy run 68's `docs/bashy-service-design.md`: its execution-journal
  finding attributes the 13:15:13/13:15:37 bootouts to a register test. The
  dangerous repo path is register -> activatePairedServiceForPath -> platform
  Install -> preflightTakeover -> removeManagedRegistrations -> launchctl
  bootout of the real label. HOME/XDG changes do not isolate launchd's GUI
  domain. `TestRegisterScriptedLegacyGate` already has a local fake in commit
  61da765 (13:16Z); relying on each test remembering that fake was insufficient.
- All launchd/systemd/Windows registration and `run` commands now use an
  injected service-command boundary. TestMain rejects unmocked calls before
  exec; ignored command errors cannot conceal a violation. Power/linger/reboot
  runners also have an injected boundary, including service install's power
  step that runs before registration. Existing postinstall unit tests already
  use injected runners.
- The sole outpost-e2e-bin builder now requires `outpost_testisolated`. That
  build permanently rejects service-manager calls, uses a per-test service
  identity, and writes attempted calls to a per-test audit checked at cleanup.
  No PATH fallback to a real manager is possible through these boundaries.
  Explicit platform tests use a harmless test-executable fake and unique label.
- Also fixed unpair's separate unconditional uninstall: a non-default scratch
  config now leaves the host service alone, matching pairing's existing gate.
  The final unpair output no longer claims a service was removed when skipped.
- Red: TestDeactivatePairedServiceScratchConfig recorded one fake uninstall;
  TestServiceCommandBoundary reported direct exec bypasses on all platforms
  and the missing isolated-build tag. Neither red ran a real service manager.
- Green: targeted race gate, `go test -race -tags fb_archives ./cmd/outpost
  -run '^(TestService.*|TestRegisterServiceGuard|TestRegisterScriptedLegacyGate|TestActivatePairedService.*|TestDeactivatePairedService.*|TestRender.*|TestDaemonProcessCleanup)$'
  -count=1 -timeout=60s`. Registration, uninstall, power-policy, source-boundary,
  and failure/timeout cleanup guards passed. Separate race gate with tags
  `fb_archives outpost_testisolated` passed the service guards and audit test.
  The isolated runner was tested with test-executable subprocesses, not a daemon.
- Rechecked executable paths using macOS proc_pidpath: zero matching test
  daemons remained. No additional processes were signaled. No real service
  manager, real-daemon E2E, Podman, VM, or full-suite run was performed.

Restart regression follow-up (2026-11-10, review steering):

- The SSHD tests call the MCP restart closure, which reaches execSelfStart.
  With OUTPOST_SUPERVISED=1 this exits for a supervisor to respawn; the test
  harness previously supplied no supervisor. This path does not invoke a
  service manager, so injecting a launchctl fake would not restore the listener.
- Both SSHD tests now use a test-owned restart helper: wait for the old daemon,
  reap its group, start a replacement with the same isolated environment and
  per-test service identity, register cleanup/watchdog, and await readiness.
  The default hard service-manager guard remains intact.
- Red/green: TestDaemonProcessRestart failed with "restart did not replace the
  supervised process" before the helper implementation; it now passes using
  harmless shell subprocesses and checks replacement, wait, and environment.
- Targeted race gate passed with fb_archives: TestDaemonProcess.*, TestService.*,
  TestRegisterServiceGuard, TestRegisterScriptedLegacyGate,
  TestActivatePairedService.*, TestDeactivatePairedService.*, TestRender.*.
  A second race gate with fb_archives,outpost_testisolated passed
  TestIsolatedServiceAudit, TestService.*, and TestDaemonProcessRestart.
  No actual E2E, Podman, VM, or full-suite tests were run, per host restriction;
  the SSHD integration test itself remains for the permitted validation host.
- Revalidated and killed one additional orphan (PID 26670, parent 1) whose
  proc_pidpath executable was inside the host temp outpost-e2e-bin-904575739
  directory; confirmed its executable mapping disappeared. No real host
  service was targeted.
- Corrected the service-isolation commit to carry only the requested Adhoc
  trailer; this follow-up likewise uses only Adhoc.
