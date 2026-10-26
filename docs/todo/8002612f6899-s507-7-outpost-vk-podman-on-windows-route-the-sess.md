---
id: 8002612f6899
kind: bug
title: 'S507.7 outpost vk-podman on Windows: route the session app to the published port when the podman machine is not ''podman-machine-default'' (still 502 after 52b3803)'
seq: 2726
status: done
priority: p0
created: 2026-10-26T05:47:51.778198Z
weave: 99
assignee: codex-gpt6-sol
sprint: 507
sprint_id: 77d7ab0f-6aef-5bd3-876d-3f50e9b585ea
sprint_title: 'YC demo blockers: four bug fixes (dks reattach, executor image default, AMD VRAM on Windows, kubectl /c/ paths)'
closed: 2026-10-26T06:15:36.936277Z
closed_by: s507-manager
---

Live on puppy 2026-10-25 23:00 PDT with outpost c23abbe (includes 52b3803): reattach still HTTP 502. On puppy: podman 6.0.0; 'podman ps' shows the session container 'outpost-6bcc4857-run' published as 0.0.0.0:45563->45563/tcp; 'podman machine inspect' -> 'Error: podman-machine-default: VM does not exist' (the machine has another name or podman runs via another provider); 'outpost apps list' shows no vk-user transient app line (transient apps may be separate). So the machine-address lookup in transient_machine_windows.go likely fails and the target is wrong. Fix: resolve the active machine (podman machine list / default connection) or simply target 127.0.0.1:<published host port> when podman publishes 0.0.0.0:<port> on the Windows host; log the chosen target. Acceptance (s507-manager runs it live): session from Dragon -> reattach from elephant shows replay + exit 7. Bug fix only.
