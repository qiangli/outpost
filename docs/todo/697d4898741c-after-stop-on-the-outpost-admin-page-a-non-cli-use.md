---
id: 697d4898741c
kind: bug
title: After Stop on the outpost admin page a non-CLI user has no way to start outpost again (admin UI goes down with the daemon; supervisor only starts at boot)
seq: 2772
status: todo
priority: p2
labels:
    - windows
    - admin
    - ux
created: 2026-11-11T11:01:41.017175Z
---

Operator, Windows test host, 2026-11-11 02:45: Stop on the outpost admin page shut the daemon down; it stayed down (correct for 'stop'), and the admin UI went with it. The operator then ended the remaining outpost processes in Task Manager, which removed the supervisor too; it only starts again at boot (scheduled task 'outpost supervisord', boot trigger). Restored by the S605 conductor with Start-ScheduledTask; daemon took >2.5 min to serve the admin port. Gap: there is no user-facing Start (by design the tray only watches outpost). Options for outpost: a Start shortcut / Start-menu entry that runs the supervisor task (or 'outpost enable' via the supervisor), or have the admin Stop keep a minimal start endpoint. Related: rc.30 auto-upgrade on that host loops received -> probe_failed every 10-15 min since 09:30Z (outpost.exe.upgrading staged, never swapped) - check with bashy todo 5c1def05 (Windows bashy start hang under Task Scheduler).
