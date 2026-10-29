---
id: 86730587f93a
kind: bug
title: 'outpost register --out: still overwrites the ACTIVE pairing''s recovery_code.txt and prints ''Restarting the running outpost'' though it does not'
seq: 2759
status: todo
priority: p2
labels:
    - pairing
    - profiles
    - windows
created: 2026-10-29T01:58:37.836343Z
---

Found 2026-10-28 (Sprint 522, adding zoo-aws profiles on puppy/elephant via outpost-profile add, which runs 'outpost register --out <profile>/agent.json'): the profile agent.json is written to --out, but the new pairing's recovery code is stashed to the DEFAULT config dir's recovery_code.txt, replacing the active satellite's code; and the command prints 'Paired. Restarting the running outpost to apply' although the daemon keeps the active pairing (S514.5 finding 8). Fix: with --out, stash the recovery code next to the --out file and do not print/perform the restart message.
