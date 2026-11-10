---
id: cf9c21cf073f
kind: bug
title: outpost tests leak real 'outpost start' daemons on the dev box
seq: 2765
status: todo
priority: p1
labels:
    - test
    - hygiene
created: 2026-11-10T15:15:35.196482Z
assignee: codex-gpt6-astra
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
