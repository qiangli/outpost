---
id: d864610d2e1f
kind: feature
title: 'S498.4 outpost move: leave cluster, re-pair to another owner''s satellite with their invite, best-effort delete the old record, land as an orphan (re-pair never auto-joins a cluster)'
seq: 1907
status: done
priority: p1
created: 2026-10-24T03:44:21.129566Z
assignee: muse-conductor
sprint: 498
sprint_id: 9c13d93e-b866-5ba6-bf54-f3e5b2e47f18
sprint_title: 'Satellite ownership, sharing and prune (launch MVP): two logins, satellite transfer, bashy host/satellite prune'
closed: 2026-10-25T10:12:34.90896Z
closed_by: s498-manager
---

Spec: umbrella docs/satellite-ownership-sharing-prune-mvp.md section 2 (host move). Repo: outpost (plus a tiny cloudbox endpoint only if no self-delete path exists; then file that as a sibling cloudbox run). Deliver: 'outpost move --to <server URL> --code <invite> [--name N]': (1) if the host is in a cluster, run the existing cluster clear/leave path first; (2) register with the new server and invite (same as 'outpost register', service restart included); (3) best-effort delete this host's record on the old server using its old token (skip with a note if unsupported; prune covers it); (4) after any re-pair (move or register), do NOT re-issue the cached cluster join; the host stays an orphan until the owner joins it explicitly. Absorbs 497 story bd44ff51 (lern auto-joined ivy's cluster). Red/green: move with and without a cluster; no auto-join after re-pair; old-record delete attempted. Live: move a test host (not lern) between two of the operator's satellites; verify it lands as an orphan on the new one and the old record is gone or listed by host prune.

## Rework 1 (s498-manager, 2026-10-24). Start from refs/salvage/abandoned-85 commit ff3eb63 (`git cherry-pick ff3eb63`; DROP e1b27c1, the KB-only commit).
Gate passed (3-OS build, cmd/outpost + admincore tests). One fix: outpost is a PUBLIC repo; real host/satellite names must not appear. Replace every "lern", "ivy", "lern-on-ivy" in code comments/tests/docs added by ff3eb63 with neutral wording (e.g. "a third-party host re-paired to another owner's satellite silently joined its cluster"). `git diff origin/main | grep -niE 'lern|ivy'` must be empty. No other changes. Re-run: go build ./... (linux/darwin/windows) and go test -short -tags fb_archives ./cmd/outpost/ ./internal/agent/admincore/. One commit with this story's trailers.
