---
id: 1119b8f09f62
kind: bug
title: 'P0 outpost: never apply an older build on any upgrade path (--direct picks newest by date) + auto-rollback always on (elephant bricked 2026-10-24)'
seq: 1909
status: done
priority: p0
created: 2026-10-24T08:46:31.195236Z
assignee: muse-conductor
sprint: 497
sprint_id: 16af0380-7ec7-5d41-a9c6-50e1b03cdf11
sprint_title: 'Fix the 2026-10-23 findings: satellite login/superadmin security, web console multi-satellite UX, fleet downgrade trap, Windows agent bugs, weave/sprint tooling, outpost headless/version skew'
closed: 2026-10-24T18:39:35.123393Z
closed_by: s497-manager
---

TOP PRIORITY (operator 2026-10-24): elephant was bricked: `outpost upgrade --direct` resolved GitHub "latest" = v0.32.0 (2026-10-08) over a newer rc.10-dev build, swapped, and the host never came back because auto-rollback is opt-in. Outpost side of umbrella stories a9598d7f (auto-rollback always on) + the --direct part of the downgrade trap (b756ce4d/5179741f). One repo: outpost.
Deliver:
1. NEVER apply an older build, on every upgrade path (CLI `outpost upgrade` with --direct, --from, --local, the fleet puller, the cloudbox push): one shared guard (reuse/extend internal/agent/upgrade shouldDeclinePulledTarget: semver + prerelease-base rule + build/commit time when known). For --direct, resolve the NEWEST release by published date among releases whose version is strictly newer than the running build (GitHub "latest" ignores prereleases and is the bug), and pass its published_at as the candidate build time. Refuse with a clear message naming both versions. `--force` keeps its current meaning (same commit) and does NOT allow downgrades. Deliberate rollback stays possible only through the existing rollback/ledger path (document which command).
2. Auto-rollback ALWAYS ON: remove the `outpost builtins --auto-rollback on|off` flag and the FileConfig.AutoRollbackOn() gate (no deprecation path; drop the config key); the watchdog (cmd/outpost/supervisord_watchdog.go revert + upgrade.ArmConfirm confirm) always reverts a just-upgraded build that does not confirm healthy, and records the failed candidate in the upgrade ledger (quarantine). The CLI upgrade path must arm the same confirm/rollback as pushed upgrades.
3. Verify (unit-level) that the Windows outpost-boot (Task Scheduler S4U) path starts the supervisor so the watchdog can act; if it does not, make it.
Tests (red first): --direct with releases [v0.32.0 stable 2026-10-08, v1.0.0-rc.12-dev 2026-10-23] on a v1.0.0-rc.10-dev host picks rc.12 and never v0.32.0; a candidate older than the running build is refused on --from/--local/--direct/puller; a candidate that fails to confirm is reverted with auto-rollback and no flag exists. Use httptest fakes and temp HOME/XDG; never touch real services or binaries.
Gate: go test -short -tags fb_archives ./cmd/outpost/ ./internal/agent/upgrade/... + go build ./... linux/darwin/windows. No hostnames in code/tests (use example names). Commit with this story's trailers only.

## Rework 1 (s497-manager, 2026-10-24). Run #90 gate GREEN (3-OS build, vet, cmd/outpost + upgrade + conf + admincore + mcpapi tests). Start from refs/salvage/abandoned-90 (commit 91680b6): cherry-pick it onto current main, then ONLY: outpost is a PUBLIC repo; remove the real host name "elephant" from every comment, test name and doc added by 91680b6 (6 places, e.g. "the elephant brick" -> "the 2026-10-24 downgrade incident"; TestDirectPicksNewestNewerRelease comment likewise). `git diff origin/main | grep -niE 'elephant|lern|ivy|alien|novi|dragon'` must be empty. No other changes. Re-run go build ./... (linux/darwin/windows) and go test -short -tags fb_archives ./cmd/outpost/ ./internal/agent/upgrade/... . One commit with this story's trailers.
