---
id: 6935bc262d51
kind: bug
title: 'Hosted DKS control plane fails to stabilize on macOS: metrics-server RBAC forbidden, then etcd peer IP mismatch on restart'
seq: 1897
status: todo
priority: p1
labels:
    - dks
created: 2026-10-19T02:17:18.290239Z
sprint: 454
sprint_id: 81c8d9ad-b9a7-5a08-9f9f-a4e5fed32098
sprint_title: 'Post-S443 must-fixes: cloudbox tier-5 plane green, zot config reapplied on every start, Bash# tour green'
---

Observed 2026-10-18 on a macOS test host (podman machine), Sprint 446 S18 live leg: a disposable hosted outpost DKS control plane started from a freshly built signed scratch runtime image. First container exited at startup because metrics-server bootstrap hit RBAC forbidden (bootstrap race); the restarted control plane then failed with an etcd peer IP mismatch (container IP changed across restart, 10.88.0.22 vs 10.88.0.23) and /readyz stayed 503. A direct k3s from the SAME image ran fine (kubectl get pods -A ok). Expected: hosted DKS reaches ready on first start and survives a restart with a changed container IP. Fix with a reproducing test of the bootstrap order and etcd advertise address on restart.

2026-10-19 reconciliation: attach this existing defect to Sprint 454; no duplicate bug. Blocks the remaining Sprint 450 S18 peer-hosted DKS locator proof. Preserve first-boot metrics-server RBAC and changed-container-IP etcd restart reproductions; verify authenticated kubectl reads and namespace cleanup, not serving=true alone. Fix belongs to known DKS defects, not a new feature in the master/satellite refactor. Same released Outpost must subsequently pass ordinary endpoint compatibility against standalone and satellite. No implementation/live resource authorization in this session.
