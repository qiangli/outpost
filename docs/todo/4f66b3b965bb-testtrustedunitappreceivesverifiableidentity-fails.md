---
id: 4f66b3b965bb
kind: bug
title: TestTrustedUnitAppReceivesVerifiableIdentity fails on main (403 untrusted identity)
seq: 2777
status: done
priority: p2
labels:
    - test
created: 2026-11-16T11:30:07.239372Z
closed: 2026-11-16T12:30:54.67964Z
---

Pre-existing failure, found 2026-11-16 while bumping the File Browser fork pin (umbrella todo eedbeea2); not caused by that bump. Reproduced on unchanged origin/main in a clean worktree, macOS dev box, GOWORK=off go test -short ./cmd/outpost: services_test.go:130: GET through the published route = 403, "untrusted identity"; want 200. It is the only failure in cmd/outpost -short. Last test-file change: a29d628 (2026-11-12, apps: enforce the host grant on cloud-proxied require_login requests), a likely candidate to bisect. Root cause not yet identified: bisect, classify (product/test/env) and record before any fix.

Root cause (2026-11-16, Sprint 648 conductor): NOT pre-existing. Introduced by 8f0d06e (cloud identity is signed only for tunnel-ingress requests); bisect: d9daa01 ok, 8f0d06e FAIL. Test defect: the test simulated a cloud arrival with only X-Forwarded-Prefix on a plain listener, which is exactly the local forgery 8f0d06e now refuses. Fix: the test's proxy marks connections with agent.TunnelIngressConnContext, like the real ingress. The product behavior is intended.
