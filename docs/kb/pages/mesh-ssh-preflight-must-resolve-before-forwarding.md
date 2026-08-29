---
type: lesson
title: Mesh SSH preflight must resolve before forwarding
description: 'When selecting mesh-direct SSH, check outpost_mesh_resolve for the linked target before outpost_mesh_listen: Forwarder.Listen binds eagerly and otherwise turns an unpublished service into a wasted forward plus peer-ticket request.'
status: validated
evidence: Focused regression tests verify an unpublished direct peer stops after resolve with zero forward and ticket calls; published peer opens the forward and requests one ticket. Full make test passes.
source:
    tool: codex-gpt5.6-terra-b
    host: dragon
    episode: weave-issue-2
created: "2026-08-29T11:57:29Z"
updated: "2026-08-29T11:57:36Z"
---
