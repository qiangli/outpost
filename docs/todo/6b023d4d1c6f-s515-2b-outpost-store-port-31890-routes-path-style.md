---
id: 6b023d4d1c6f
kind: feature
title: S515.2b outpost store port 31890 routes path-style s3-<peer-id> buckets to peer s3
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

Round 3 supersedes the Host-routing design from rounds 1-2. Spec: umbrella docs/sprint-515-satellite-loom.md, Stable peer storage address (decided 2026-10-27). Route the first path segment s3-<peer-id> to local s3 for self, or the peer s3 mesh service (existing LAN-first transport). Preserve Host, path, query and streaming bodies. No HTTP Host-label compatibility routing. Malformed/unknown buckets return 404 with a message; remote without mesh returns 502 with a clear message. Gate: go test -short -tags fb_archives ./internal/agent/storeroute ./cmd/outpost && go build ./cmd/outpost/. Tests cover presigned GET preservation, list-bucket GET, remote without mesh and bad buckets. Companion of S515.2 (yoke 5434b5be); rounds 1-2 delivered in outpost 386fa43 and a0c7791. Live Windows integration remains with the sprint manager.
