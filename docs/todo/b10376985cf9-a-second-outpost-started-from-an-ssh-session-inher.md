---
id: b10376985cf9
kind: bug
title: a second outpost started from an ssh session inherits OUTPOST_SUPERVISED=1 and exits on its restart path with no supervisor; with a shared binary path its upgrade puller can swap the main daemon's binary
seq: 1904
status: todo
priority: p2
created: 2026-10-23T20:17:51.077147Z
---

Found 2026-10-23 on a Linux host: an outpost started from a shell spawned by the supervised main outpost inherited OUTPOST_SUPERVISED=1. When its upgrade puller accepted the fleet target, execSelfStart did os.Exit(0) ('supervised - exiting for the supervisor to respawn') and the instance was gone. Workaround: unset OUTPOST_SUPERVISED and run from a private binary copy. Expected: OUTPOST_SUPERVISED is stripped from shell/exec sessions served by the daemon (or the supervisor passes a token that a child cannot inherit), and a non-default config dir never self-upgrades a binary shared with another instance.
