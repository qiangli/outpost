---
id: 6b023d4d1c6f
kind: feature
title: 'S515.2b outpost half of S515.2: fixed local store port 31890 routes <peer-id>.<sat-id>.localhost by Host to that peer''s s3 mesh service (LAN first, else mesh), Host passed through unchanged'
seq: 2758
status: done
priority: p0
labels:
    - loom
created: 2026-10-27T16:46:30.864018Z
weave: 103
assignee: claude-sonnet5.5
sprint: 515
sprint_id: e82160ea-95db-5f2a-b69e-b2d784b2996b
sprint_title: 'satellite loom: private forge + CI + groups (KISS/MVP, reuse loom/act_runner/scopes)'
closed: 2026-10-27T16:55:31.41377Z
resolution: fixed
closed_by: claude-sonnet5.5-y
---

Companion of S515.2 (yoke 5434b5be, merged locally as yoke 259a9d82): one repo per run, so the outpost code lives here. Spec: umbrella docs/sprint-515-satellite-loom.md "Stable peer storage name"; decision note yoke docs/peer-store-name.md.

Do (KISS, reuse outpost mesh + services):
- Every outpost listens on 127.0.0.1:31890 (yoke loom.DefaultStorePort; register it wherever outpost declares fixed ports so ports_lint_test passes).
- Route by Host header: <peer-id>.<sat-id>.localhost[:31890] -> that peer's `s3` mesh service (the same service `outpost mesh dial` reaches); prefer the LAN address, else the mesh tunnel. Unknown host -> 404/502 with a clear message.
- Pass Host through UNCHANGED (SigV4 presigned URLs sign Host). Stream bodies; no buffering.
- Local peer (peer-id = self) routes to the local s3 service directly.
Test: focused red/green unit test with a fake s3 backend: Host-routed request reaches the right backend with the same Host and a presigned-style query string intact; unknown peer errors. Gate: go test -short ./<touched packages>.
Live (manager runs it on the sprint card hosts after merge): image pull + artifact download via loom SERVE_DIRECT come from the peer.
No host names in this OSS repo. Commit trailers: Sprint: 515 / Story: S515.2b / Story-ID: 6b023d4d1c6f.
