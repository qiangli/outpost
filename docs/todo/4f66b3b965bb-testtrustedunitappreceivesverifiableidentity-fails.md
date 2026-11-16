---
id: 4f66b3b965bb
kind: bug
title: TestTrustedUnitAppReceivesVerifiableIdentity fails on main (403 untrusted identity)
seq: 2777
status: todo
priority: p2
labels:
    - test
created: 2026-11-16T11:30:07.239372Z
---

Pre-existing failure, found 2026-11-16 while bumping the File Browser fork pin (umbrella todo eedbeea2); not caused by that bump. Reproduced on unchanged origin/main in a clean worktree, macOS dev box, GOWORK=off go test -short ./cmd/outpost: services_test.go:130: GET through the published route = 403, "untrusted identity"; want 200. It is the only failure in cmd/outpost -short. Last test-file change: a29d628 (2026-11-12, apps: enforce the host grant on cloud-proxied require_login requests), a likely candidate to bisect. Root cause not yet identified: bisect, classify (product/test/env) and record before any fix.
