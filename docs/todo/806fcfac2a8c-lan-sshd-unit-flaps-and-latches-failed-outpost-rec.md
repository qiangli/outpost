---
id: 806fcfac2a8c
kind: bug
title: 'LAN sshd unit flaps and latches failed: outpost reconciler and bashy service monitor fight over internal units'
seq: 2778
status: todo
priority: p1
created: 2026-11-16T12:29:07.223615Z
---

Observed 2026-11-16 on the Windows test host and the macOS dev box: the sshd unit (internal, owner outpost) shows enabled but failed, 'start limit hit: 5 starts within 5m', so LAN ssh on :2222 is down. After 'bashy service restart sshd' outpost logs 'service state unit=sshd starting->active' plus 'sshd: listening [::]:2222', then about 4 s later 'service withdrawn from the tunnel and mesh unit=sshd reason=unit state inactive'. The monitor records 'owner went inactive', backs off, retries, and latches after 5 cycles. Mechanism (unverified): outpost applyInternal runs a unit only while the monitor's Desired() lists it (wanted = Enabled && desired), and readStates/effectiveState take the monitor's status. While the monitor is in backoff its want is false, so outpost deactivates and reports inactive, and the monitor counts another failed start. Bisect lead: outpost 18b79ab 'fix: require monitor admission for internal unit activation'; compare bashy 8606f379 / 7d95bffa. Contributing: (1) an owner restart storm counts against unit start limits (on 2026-11-15 the test host's outpost exited status 1 within ~100 ms 4,120 times between 05:23 and 18:48 local; the cause is not in daemon.log); (2) noteLink dedupes on a single last message, so several units with different link errors log 7 WARN lines every 5 s; together with the per-minute OTLP export failure this has grown daemon.log to 4.4 GB with no rotation; (3) on that host the pure-Go grep lacks -a, which makes log forensics fail silently. Workaround on the test host: a hand-run 'outpost sshd --addr :2222' while the unit is latched. Root-cause before fixing: reproduce with an internal unit plus a monitor in backoff, then bisect.
