---
id: 799526dffabb
kind: bug
title: 'TestSeededBuiltinInternalAndCustomShareOneMachinery fails on main: bashy-mcp seeded default-on vs golden'
seq: 2779
status: todo
priority: p2
labels:
    - test
created: 2026-11-16T18:29:10.380432Z
---

Pre-existing, found 2026-11-16 while adding the browser-hub listing regression (S529.10); reproduced on unchanged origin/main in a clean worktree (GOWORK=off go test ./internal/agent/svcunit): store_test.go:245 seeded unit bashy-mcp default Enabled=true, want false (golden default-on set lacks it). Root cause not identified: decide whether yoke's bashy-mcp unit became default-on on purpose (then update the golden) or by mistake (then fix the unit); record before changing either.
