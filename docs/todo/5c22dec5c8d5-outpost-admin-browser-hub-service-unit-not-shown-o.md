---
id: 5c22dec5c8d5
kind: bug
title: 'outpost admin: browser-hub service unit not shown (operator cannot start it from the admin as yoke 68a20e27 intended)'
seq: 2771
status: done
priority: p2
labels:
    - admin
    - services
created: 2026-11-11T09:58:27.020118Z
closed: 2026-11-16T18:30:54.101065Z
---

Operator report 2026-11-11 (S605 tray review): the browser hub (bashy browser --mode live hub, ws 127.0.0.1:58082) does not appear on the outpost admin page. yoke 68a20e27 added it as an opt-in baseline service unit (pkg/fleet/baseline/services/browser-hub.yaml, enabled: false) 'to start from outpost admin'. Not yet root-caused: first establish where the admin page's service list comes from (adminui/ui/index.html + the status/services API), whether disabled baseline units are listed at all, and which outpost/bashy build the host ran (the Windows test host was rc.23, older than 68a20e27; rc.27+ carries it). Not a tray item by operator decision (the tray only watches outpost).

Root cause (2026-11-16, S529.10): environment, not code. The admin Services list is admincore.ServiceStatus -> svcunit.Store.All -> fleet.Catalog.Services() over the yoke catalog compiled into the outpost binary; its only filter is IsRetired, and the UI renders every row (disabled ones dimmed). The reporting Windows host ran rc.23, whose bashy pins yoke 34dab449, older than 68a20e27 (the commit adding browser-hub.yaml), so that binary had no unit to list. Verified: the current outpost on the dev box lists browser-hub (outpost services: loopback:58082, embedded). Fix: none in code; upgrade the host. Guard: svcunit TestAllListsTheEmbeddedBrowserHub. Also: browser-hub is now default-on with a logical gate (yoke 44e3a3e6), and the tray may flip that gate (operator decision S529.10).
