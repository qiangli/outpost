---
id: 067a1ef4746f
kind: bug
title: 'S507.8 outpost on Windows: ''cluster userkubeconfig --output /c/Users/...'' (MSYS path, e.g. from mktemp in bashy) writes to C:\c\Users\... and leaves the real path empty, so kubectl reads an empty kubeconfig and falls back to localhost:8080'
seq: 2727
status: done
priority: p0
labels:
    - windows
created: 2026-10-26T06:16:44.201067Z
assignee: agy-gemini3.8-flash
sprint: 507
sprint_id: 77d7ab0f-6aef-5bd3-876d-3f50e9b585ea
sprint_title: 'YC demo blockers: four bug fixes (dks reattach, executor image default, AMD VRAM on Windows, kubectl /c/ paths)'
closed: 2026-10-26T06:35:24.046715Z
closed_by: s507-manager
---

Found 2026-10-25 by s507-manager verifying ab89b3bd on elephant: T=$(mktemp) -> /c/Users/liqia/AppData/Local/Temp/tmp.X; 'outpost cluster userkubeconfig --output "$T"' prints 'wrote /c/Users/...' but the file at C:\Users\...\tmp.X is 0 bytes; the content lands in C:\c\Users\liqia\AppData\Local\Temp\tmp.X. Fix (KISS, bug only): on Windows, normalise an MSYS drive path (/c/... -> C:\...) for outpost's own path flags at the point the file is opened (userkubeconfig --output at minimum; reuse an existing helper if outpost has one, else a tiny one with a focused test). No new flags or commands. Live check: on elephant, mktemp path -> --output -> wc -c > 0 at the real path and no C:\c tree written.
