---
id: d65ca7e58b35
kind: bug
title: 'S507.6 outpost vk-podman on Windows: a session pod''s attach port is not reachable from Outpost (edge 502 Bad Gateway)'
seq: 2725
status: done
priority: p0
created: 2026-10-26T05:29:22.389828Z
assignee: codex-gpt6-sol
sprint: 507
sprint_id: 77d7ab0f-6aef-5bd3-876d-3f50e9b585ea
sprint_title: 'YC demo blockers: four bug fixes (dks reattach, executor image default, AMD VRAM on Windows, kubectl /c/ paths)'
closed: 2026-10-26T06:16:22.998876Z
closed_by: s507-manager
---

Found 2026-10-25 after S507.5 (outpost 65b4658 on puppy): a dks session from Dragon now schedules on puppy-vk-podman (os=linux), pod Running, annotation dhnt.io/attach-port=46390; reattach from elephant with the account token (yoke c02f9c44) gets HTTP 502 from /matrix/h/puppy/app/vk-user-...-bashy-run-.../attach, even after the pod started. Likely the container port is published inside the podman machine (Linux VM) but the vk app target is 127.0.0.1:<port> on the Windows host. Fix: make the vk-podman transient app target reachable on Windows (publish the session port to the host through the podman machine, or target the machine address). Acceptance: on zoo, session from Dragon -> reattach from elephant shows full replay and exit code 7. Bug fix only. Live hosts: puppy (key login as liqiang via ssh -p 2222 puppy.local), elephant; ask s507-manager for host access (claims held).
