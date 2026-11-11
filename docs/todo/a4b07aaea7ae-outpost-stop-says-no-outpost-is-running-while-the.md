---
id: a4b07aaea7ae
kind: bug
title: outpost stop says 'No outpost is running' while the daemon serves the admin port (pidfile missing; no fallback)
seq: 2769
status: todo
priority: p2
labels:
    - windows
    - cli
created: 2026-11-11T09:13:46.681264Z
---

Found 2026-11-11 during the S605 tray smoke on the Windows test host (outpost v1.0.0-rc.23-dev under supervisord): the daemon (running since 10/30, admin 127.0.0.1:17777 answering) had no ~/.cache/outpost/outpost.pid, so every 'outpost stop' printed 'No outpost is running.' and exited 0 while the daemon kept serving; it only wrote supervisord.stop. The pidfile was most likely removed earlier by a stale v0.32.0 outpost (old scoop bashy install, since removed) whose stop path removes the pidfile on its own liveness verdict; not proven.

Gap: runStop trusts only the pidfile. When it is missing but the admin port answers (the same probe --local-presence uses), stop should find the owner (admin endpoint / supervisor's child) and stop it, or at least exit non-zero with 'daemon is running but its pidfile is missing; restart it'. Exit 0 + 'No outpost is running' is wrong.

Repro: start the daemon, delete outpost.pid, run outpost stop.
