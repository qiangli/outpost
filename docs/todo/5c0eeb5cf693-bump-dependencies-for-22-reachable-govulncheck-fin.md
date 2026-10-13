---
id: 5c0eeb5cf693
kind: security
title: Bump dependencies for 22 reachable govulncheck findings
seq: 1891
status: todo
priority: p2
labels:
    - release
    - security
created: 2026-10-13T04:20:55.804644Z
---

Go 1.27.2 has 0 reachable standard-library findings. Reachable third-party findings: GO-2026-5062, GO-2026-5066, GO-2026-5970, GO-2026-6061, GO-2026-6099, GO-2026-6103, GO-2026-6163, GO-2026-6165, GO-2026-6213, GO-2026-6214, GO-2026-6303, GO-2026-6348, GO-2026-6354, GO-2026-6355, GO-2026-6505, GO-2026-6508, GO-2026-6603, GO-2026-6610, GO-2026-6611, GO-2026-6612, GO-2026-6615, GO-2026-6617. Bump dependencies, refresh consumer pins, and rerun the Bashy release checker until reachable=0.
