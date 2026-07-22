---
id: 5e8c9b2dad61
kind: task
title: register the QA scheduler under 'supervised' on novicortex and puppy
seq: 4
status: todo
priority: p2
created: 2026-07-22T06:12:48.293339Z
---

v0.14.4 added a 'supervised' list to agent.json: supervisord keeps operator-declared programs alive alongside the daemon, so anything it owns returns after a reboot (the OS service starts supervisord at boot).

Blocked on both hosts upgrading to v0.14.4+ FIRST. An older binary rewrites agent.json on save and silently drops fields it does not know, so adding 'supervised' before the upgrade erases it — hit during the smoke test.

puppy (Windows) — the motivating case. Its bashy scheduler died ~2026-07-11 and stayed dead until 07-21; 'bashy schedule start' registers no OS service, so it does not survive a reboot. A stopped poller is indistinguishable from one with nothing to do, which is why 10 days passed unnoticed. Entry:

  "supervised": [{"name": "qa-scheduler",
                  "path": "C:/Users/liqiang/AppData/Local/outpost/bashy.exe",
                  "args": ["schedule", "daemon"]}]

novicortex (macOS) — same treatment for its scheduler once upgraded. Note it was last seen running a dirty local build; it needs a clean v0.14.4 first.

Also relevant: 'outpost run' cannot serve Windows at all (macOS-only, launchctl), which is why supervised exists.
