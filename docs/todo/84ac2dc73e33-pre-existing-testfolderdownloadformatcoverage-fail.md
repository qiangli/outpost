---
id: 84ac2dc73e33
kind: bug
title: 'Pre-existing: TestFolderDownloadFormatCoverage fails on main (cmd/outpost)'
seq: 2766
status: todo
priority: p2
labels:
    - test
created: 2026-11-10T15:49:02.034016Z
---

Fails on main b44aa19 on the dev box (macOS): 'go test ./cmd/outpost/ -run TestFolderDownloadFormatCoverage'. Pre-existing, found while verifying the test-isolation fix (todo cf9c21cf); not caused by it.
