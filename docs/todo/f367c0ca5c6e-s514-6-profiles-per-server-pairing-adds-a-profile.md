---
id: f367c0ca5c6e
kind: feature
title: 'S514.6 profiles per server: pairing adds a profile; ''outpost use <server>'' and a UI picker switch by restarting outpost'
seq: 2730
status: todo
priority: p0
labels:
    - pairing
created: 2026-10-27T02:16:25.650625Z
sprint: 514
sprint_id: fadd1a46-0368-50f1-a65d-4ed147b17629
sprint_title: 'invitation email + scoped satellites (KISS/MVP: scope + expiry on the one-time pairing code; no outpost change)'
---

Minimal, no core change (operator 2026-10-26). Each server an outpost has paired with is saved as its own profile (config + credentials); pairing with a new server writes a new profile instead of overwriting the current one; existing installs become a single profile. 'outpost use <name>' selects the active profile and restarts outpost through its existing restart path; the outpost UI shows a server picker that does the same. One active server at a time (not simultaneous connections). Red/green: two profiles; use switches the active config after restart; pairing again does not clobber the other profile.
