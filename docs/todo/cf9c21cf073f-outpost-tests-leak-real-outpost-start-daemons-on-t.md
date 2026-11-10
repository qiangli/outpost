---
id: cf9c21cf073f
kind: bug
title: outpost tests leak real 'outpost start' daemons on the dev box
seq: 2765
status: todo
priority: p1
labels:
    - test
    - hygiene
created: 2026-11-10T15:15:35.196482Z
assignee: codex-gpt6-astra
---

On the dev box many processes '<tmp>/outpost-e2e-bin-*/outpost start' were running (some 20 days old, several from 2026-11-10 worker test runs). Tests that build and start a real outpost daemon must stop it (t.Cleanup kill of the process group + wait) even on failure/timeout. Find every such test (grep outpost-e2e-bin), fix cleanup, add a guard test. Also reap the leaked ones on this host: only processes whose executable lives under a temp dir named outpost-e2e-bin-*; never touch the real outpost or bashy services.
