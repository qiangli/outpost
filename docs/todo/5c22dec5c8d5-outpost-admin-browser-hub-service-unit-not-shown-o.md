---
id: 5c22dec5c8d5
kind: bug
title: 'outpost admin: browser-hub service unit not shown (operator cannot start it from the admin as yoke 68a20e27 intended)'
seq: 2771
status: todo
priority: p2
labels:
    - admin
    - services
created: 2026-11-11T09:58:27.020118Z
---

Operator report 2026-11-11 (S605 tray review): the browser hub (bashy browser --mode live hub, ws 127.0.0.1:58082) does not appear on the outpost admin page. yoke 68a20e27 added it as an opt-in baseline service unit (pkg/fleet/baseline/services/browser-hub.yaml, enabled: false) 'to start from outpost admin'. Not yet root-caused: first establish where the admin page's service list comes from (adminui/ui/index.html + the status/services API), whether disabled baseline units are listed at all, and which outpost/bashy build the host ran (the Windows test host was rc.23, older than 68a20e27; rc.27+ carries it). Not a tray item by operator decision (the tray only watches outpost).
