---
id: 9c331c064ea6
kind: bug
title: ssh over the tunnel hangs when an older outpost client (77f7547) talks to a v1.0.0-rc.12-dev outpost server; elevation succeeds but exec never returns
seq: 1902
status: todo
priority: p1
created: 2026-10-23T20:17:46.771843Z
sprint: 497
sprint_id: 16af0380-7ec7-5d41-a9c6-50e1b03cdf11
sprint_title: 'Fix the 2026-10-23 findings: satellite login/superadmin security, web console multi-satellite UX, fleet downgrade trap, Windows agent bugs, weave/sprint tooling, outpost headless/version skew'
---

Found 2026-10-23 during the lern->ivy migration. The client is the macOS outpost 77f7547 (built 2026-10-19). Against a host running v0.32.0-dev, 'outpost ssh user@host cmd' works; against the same host after updating to v1.0.0-rc.12-dev it hangs (timeout), while 'outpost connect' elevation succeeds. A client built from outpost main (3c7ee41) works. Users with older clients lose ssh/exec to updated hosts. Expected: wire compatibility across one release, or a clear version-skew error instead of a hang.
