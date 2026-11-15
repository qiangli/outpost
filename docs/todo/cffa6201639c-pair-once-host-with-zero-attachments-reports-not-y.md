---
id: cffa6201639c
kind: bug
title: pair-once host with zero attachments reports 'Not yet configured' after the detach restart
seq: 2774
status: todo
priority: p2
labels:
    - satellite
created: 2026-11-14T14:32:41.983163Z
---

Update 2026-11-14 (fleet run 6): the twin suite passes 26/26 once the harness restarts the daemon with 'outpost stop' + 'outpost start' after re-attach, so attach itself works and the defect is narrower: after detaching the last attachment the re-exec'd daemon logs 'Not yet configured', serves no /mcp (so 'satellite attach' through the daemon fails until a restart) and 'outpost stop' does not end it (the harness also kills the process group). Still a product defect, no longer blocking S23.
