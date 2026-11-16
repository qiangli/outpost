---
id: 3f65184e094a
kind: bug
title: daemon.log grows without bound; startup crash loop leaves no error
seq: 2780
status: todo
priority: p2
created: 2026-11-16T20:05:17.499317Z
---

Found 2026-11-16 while root-causing todo 806fcfac on the Windows test host. (1) daemon.log reached 4.4 GB with no rotation; the steady noise is a per-minute 'failed to upload metrics' (OTLP export to localhost:4318 with nothing listening) plus the monitor-link spam fixed in 55b2fdd. (2) On 2026-11-15 05:23-18:48 local the supervised outpost exited status 1 within ~100 ms 4,120 times after running 5h31m on rc.35; the reason is in neither daemon.log nor supervisord.log, so early-startup errors must reach a log the supervisor keeps. (3) The restart storm counted against every internal unit's start limit (sshd/warm latched). Root-cause (2) before fixing; for (1) add size-based rotation and make the OTLP export failure log once.
