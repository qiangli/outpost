---
id: 374bb1c26b88
kind: bug
title: 'S507.5 outpost vknode: a vk-podman node on a Windows host advertises kubernetes.io/os=windows, but it runs Linux containers (podman machine), so Linux session pods never schedule there'
seq: 2724
status: todo
priority: p0
created: 2026-10-26T05:01:34.847834Z
sprint: 507
sprint_id: 77d7ab0f-6aef-5bd3-876d-3f50e9b585ea
sprint_title: 'YC demo blockers: four bug fixes (dks reattach, executor image default, AMD VRAM on Windows, kubectl /c/ paths)'
---

Found 2026-10-25 testing the reattach fix (yoke f5e634d3): session pods select kubernetes.io/os=linux + outpost.dhnt.io/backend=vk-podman; zoo's only vk-podman node, puppy-vk-podman (Windows host, podman machine), carries kubernetes.io/os=windows -> FailedScheduling '5 node(s) didn't match'. Fix: a vk-podman node advertises the OS/arch of the containers it runs (linux/<machine arch>), while vk-native keeps the host OS. Acceptance: on zoo, puppy-vk-podman shows os=linux; a dks session from Dragon schedules there and reattaches from elephant with full replay and exit code 7. Bug fix only.
