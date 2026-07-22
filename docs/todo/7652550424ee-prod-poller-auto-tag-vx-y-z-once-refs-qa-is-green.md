---
id: 7652550424ee
kind: task
title: 'prod poller: auto-tag vX.Y.Z once refs/qa/* is green, closing the last manual release step'
seq: 1
status: todo
priority: p2
created: 2026-07-22T06:10:34.726765Z
---

Promotion is the only step of the release pipeline still done by hand: something must push a bare vX.Y.Z tag to fire promote.yml. v0.14.3 and v0.14.4 were both tagged manually on 2026-07-21.

The design is already written down — scripts/qa-poller.sh's header describes it: 'The prod poller is the same shape watching refs/qa/*; it tags the release vX.Y.Z once its CONFIGURED required-OS set is present (availability-driven).' It just was never built.

No workflow change is needed. promote.yml's PRIMARY trigger is already the bare-tag push (workflow_dispatch is only the manual fallback), so a poller that creates the tag completes the loop for every repo at once — outpost, bashy and ycode all share that trigger shape.

Requirements worth pinning down before implementing:
  - Gate must match promote.yml's REQUIRED set (default 'windows'), or it will tag versions QA never cleared on the required OS.
  - Idempotent: never re-tag a version that already has a bare tag, and survive a partial run. The QA pollers get this right by checking for the ref first.
  - Availability-driven, not all-OS: the gate is whichever OS set is configured, so a fleet without a Linux QA host still promotes.
  - Fail loud. A prod poller that stops silently looks exactly like 'nothing to promote' — the same failure mode that hid puppy's dead QA scheduler for 10 days (2026-07-11 to 07-21).

Runs on a token-holding host (dragon today) on the same 15m schedule as the QA pollers. It should be registered under outpost's 'supervised' config (v0.14.4+) rather than 'bashy schedule start', so it survives a reboot — that non-persistence is exactly what killed the QA lane.
