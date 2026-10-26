---
id: 1807abf42dd1
kind: bug
title: 'outpost ssh client: exec hangs until stdin reaches EOF even after the remote command exits (and prints nothing); a session to a peer that starts a detached process stays open'
seq: 2728
status: todo
priority: p1
labels:
    - ssh
created: 2026-10-26T09:23:09.018406Z
---

Found 2026-10-26 recording the YC demo (s507-manager), outpost client built from main c10f4ca, LAN-direct key auth. (1) 'sleep 30 | outpost ssh liqiang@<peer>.local -- echo hi' -> no output, killed by timeout 20; same with </dev/null -> 'hi' rc=0. OpenSSH returns when the remote command exits regardless of stdin. Inside a terminal recorder (stdin = pty) every exec hung. (2) On a Windows peer running outpost 77a6f64, 'outpost ssh <peer> -- powershell -File launch.ps1' where the script Start-Process'es a long-running exe with redirected output never returns (the exe keeps running); the same on a peer running 2d243e0 returns at once. Workaround used: </dev/null on every call + timeout on (2).
