---
id: 1842eb57edb4
kind: bug
title: 'Windows: outpost daemon child commands flash console windows; enable starts a second daemon in the user session beside the supervisor'
seq: 2770
status: todo
priority: p1
labels:
    - windows
    - daemon
created: 2026-11-11T09:34:29.377851Z
---

Found 2026-11-11 during the S605 tray smoke on the Windows test host (outpost rc.23, supervisor = scheduled task 'outpost supervisord', S4U, session 0).

1) Console flashing: a daemon running in the interactive session spawned netsh advfirewall show currentprofile, netsh advfirewall firewall show rule name=outpost sshd, bashy.exe service status --json and powershell Get-CimInstance Win32_Processor every 2-3 s, each without CREATE_NO_WINDOW/HideWindow, so Windows Terminal (default console host) opened/closed a window each time. Under the session-0 supervisor this is invisible, which is why it went unnoticed. Fix: one exec helper for daemon children on Windows with SysProcAttr{HideWindow, CREATE_NO_WINDOW}; also question the 2-3 s cadence of those probes.

2) Duplicate daemon: with the supervisor alive (pid since 10/30) and supervisord.stop present, 'outpost enable' (run from the tray's Start, interactive session) cleared the intent AND started 'outpost start' directly in session 1 (01:28:03); the supervisor started its own daemon 9 s later, which exited 1 (admin port taken: 'outpost is already running (pid ...)'). enable must not exec a daemon when discoverInit/the supervisor is running; it should only clear the intent and wait.

Evidence: supervisord.log 01:28:12/01:29:49 restarts; process tree captured by the S605 conductor.
