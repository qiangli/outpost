---
id: f367c0ca5c6e
kind: feature
title: 'Superseded: outpost profiles are a bashy Bash# command (Sprint 514 S514.6 in bashy); no outpost change'
seq: 2730
status: todo
priority: p3
labels:
    - pairing
created: 2026-10-27T02:16:25.650625Z
---

Minimal, no core change (operator 2026-10-26). Each server an outpost has paired with is saved as its own profile (config + credentials); pairing with a new server writes a new profile instead of overwriting the current one; existing installs become a single profile. 'outpost use <name>' selects the active profile and restarts outpost through its existing restart path; the outpost UI shows a server picker that does the same. One active server at a time (not simultaneous connections). Red/green: two profiles; use switches the active config after restart; pairing again does not clobber the other profile.
