---
id: 5179741f54da
kind: bug
title: fleet upgrade puller treats release v0.32.0 (older code, 2026-10-07) as newer than v0.32.0-dev (2026-10-08) and would apply it; satellite fleet targets can be stale
seq: 1905
status: done
priority: p2
created: 2026-10-23T20:17:53.190743Z
assignee: muse-spark1.2
sprint: 497
sprint_id: 16af0380-7ec7-5d41-a9c6-50e1b03cdf11
sprint_title: 'Fix the 2026-10-23 findings: satellite login/superadmin security, web console multi-satellite UX, fleet downgrade trap, Windows agent bugs, weave/sprint tooling, outpost headless/version skew'
closed: 2026-10-24T06:38:12.371612Z
closed_by: s497-manager
---

Found 2026-10-23: the ivy satellite's fleet target is release_id v0.32.0, commit ca9a85b9 (2026-10-07). semver ranks v0.32.0 above v0.32.0-dev, so a host on the 2026-10-08 v0.32.0-dev build accepts the target and gets OLDER code (a second outpost on lern did exactly this). Expected: compare build time or commit ancestry as well as semver, or keep satellite fleet targets in lockstep with the current release. Separately, the ivy target should be refreshed to the current release.

## Worker brief (s497-manager, 2026-10-23). One repo: outpost.

Survey: internal/agent/upgrade/puller.go:158 `declinePulledTarget` uses `semver.Compare(target, cur) <= 0`; called from checkOnce (:129), fetch via fetchTarget (:172). Host metadata: agent.ReadBuildInfo() (internal/agent/version.go:182) gives Version, Commit, VCSTime, Dirty. The hub's target envelope (FleetTargetResponse: release_id, url, sha256, commit) has no time.
Fix: decline a target when the current build is a prerelease (-dev/-rc) whose base version is >= the target's base version (v0.32.0 must NOT replace v0.32.0-dev or v1.0.0-rc.12-dev), and when the envelope carries an optional commit/build time (accept an optional `build_time` field if present, RFC3339) decline when target time <= VCSTime. Never upgrade to an older build; log the decline reason. Keep existing behavior for strictly newer targets.
Tests: table test for declinePulledTarget in internal/agent/upgrade/puller_test.go (v0.32.0 vs v0.32.0-dev declined; v1.0.0 vs v1.0.0-rc.12-dev accepted; v0.33.0 vs v0.32.0-dev accepted; build_time older declined; pseudo/bare commit unchanged).
Gate: `go test -short ./internal/agent/upgrade/...` and `go build ./...` (GOOS=linux,darwin,windows). Trailers for this story only. No hostnames.
