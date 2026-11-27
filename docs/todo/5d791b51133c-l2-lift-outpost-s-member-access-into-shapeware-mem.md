---
id: 5d791b51133c
kind: task
title: L2 lift outpost's member access into shapeware/member-access/
seq: 2782
status: todo
priority: p1
created: 2026-11-27T16:53:33.640713Z
sprint: 737
sprint_id: c5dc4786-3303-5f7b-8f04-1207027a550f
sprint_title: 'Lift bashy, outpost and cloudbox into shapeware (first surfaces, beacon: 2027 positioning)'
---

Lift outpost's member access surface. Surface: pairing state, app exposure under /app/<name> with grants (owner, member by circle or group, account, anonymous), the signed relationship identity, and behaviour when the relay is unreachable. Folder: shapeware/member-access/. Describe only the public wire protocol to the coordination service; reference runs use the fake grants server pattern from bashy tests/group-shaping-e2e, never a live service.

Method: kb runbook shapeware-lift-app (bashy kb/defaults; read it first) and the format shapeware-spec-v0 §1, §4.3a and Appendix C plus the workflow standard (umbrella docs/). Lift ONE surface into shapeware/<surface>/ as a lifted surface (§4.3a): SKILL.md with the 'what it is' line and metadata.implementation / metadata.reference-commit, the two-way source step in dag.md, Build in a work copy of the repository, features.yaml, CONTRACT.md (rules first, then assertions from real runs), examples/, known issues and tbd features. Copy shapeware/index.bsh verbatim from bashy if the repo has none; add the discovery line to CLAUDE.md/AGENTS.md and .shapeware/ to .gitignore.

Acceptance (all at the delivery commit): (1) bashy conform spec check shapeware/<surface> reports nothing and bashy --bashsharp shapeware/index.bsh check passes; (2) conform cert 3 runs, none noisy, and conform mutate kills every mutant or each survivor is recorded with its reason, run on the gate host with sandbox fakes, raw logs kept; (3) the folder stands alone: copied outside any checkout, an agent that has not seen the repository reaches a passing Check from the folder's own text and lifecycle; (4) the known issues listed below are in the spec; (5) the diff touches only the spec folder, the discovery line and .gitignore. No product code changes: a defect found becomes a known issue plus a todo. Builds and tests only on the gate host, never the dev box. Commits carry Sprint/Story/Story-ID and Agent trailers.

Known issues the spec must carry: self-upgrade checks sha256 only, no signature; OS-password elevation gives the full shell, SFTP and port forwarding; app traffic and shell sessions pass through the relay and only SSH has a LAN path.

Code inspected 2026-11-27: internal/agent/grants.go, apps.go, ingress.go, routes.go, tunnel.go, auth.go; CLAUDE.md.
