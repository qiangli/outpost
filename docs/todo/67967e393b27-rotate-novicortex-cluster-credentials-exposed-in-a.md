---
id: 67967e393b27
kind: task
title: rotate novicortex cluster credentials exposed in an agent transcript
seq: 2
status: todo
priority: p1
created: 2026-07-22T06:12:33.072263Z
---

On 2026-07-21 the cluster block of novicortex's agent.json was printed in full into an agent session transcript. A redaction pass covered top-level token-ish keys but did not recurse into the nested 'cluster' object, so these were exposed in clear text:

  - cluster.node_token   (k3s node token — long-lived)
  - cluster.stcp_secret  (long-lived)
  - cluster.token        (service-account JWT — short-lived, since expired)

The JWT has rotated on its own. The node token and stcp secret are long-lived and should be rotated deliberately; do NOT record the values here.

Also worth fixing the cause: whatever dumps agent.json for humans/agents should redact recursively, not just at the top level. A nested secret is exactly the shape that slips through.
