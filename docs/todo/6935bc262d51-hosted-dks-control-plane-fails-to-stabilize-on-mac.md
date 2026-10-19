---
id: 6935bc262d51
kind: bug
title: 'Hosted DKS control plane fails to stabilize on macOS: metrics-server RBAC forbidden, then etcd peer IP mismatch on restart'
seq: 1897
status: todo
labels:
    - dks
created: 2026-10-19T02:17:18.290239Z
---

Observed 2026-10-18 on a macOS test host (podman machine), Sprint 446 S18 live leg: a disposable hosted outpost DKS control plane started from a freshly built signed scratch runtime image. First container exited at startup because metrics-server bootstrap hit RBAC forbidden (bootstrap race); the restarted control plane then failed with an etcd peer IP mismatch (container IP changed across restart, 10.88.0.22 vs 10.88.0.23) and /readyz stayed 503. A direct k3s from the SAME image ran fine (kubectl get pods -A ok). Expected: hosted DKS reaches ready on first start and survives a restart with a changed container IP. Fix with a reproducing test of the bootstrap order and etcd advertise address on restart.
