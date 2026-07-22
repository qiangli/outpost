---
id: d7bb762e7367
kind: task
title: MCP set_builtins schema omits cluster_mode, so the documented --cluster-mode flag is rejected
seq: 8
status: todo
priority: p2
created: 2026-07-22T08:55:04.181312Z
---

`outpost builtins set --cluster-mode=agent` fails against a running daemon:

  Error: validating "arguments": validating root: unexpected additional properties ["cluster_mode"]

admincore.BuiltinsParams HAS the field (admincore/builtins.go:64, ClusterMode *string `json:"cluster_mode"`) and validates it (builtins.go:357). But the MCP tool's input struct in mcpapi/tools_builtins.go exposes only `cluster *bool` — no cluster_mode — and the CLI drives the daemon through that tool, so strict schema validation rejects the property before it reaches admincore.

docs/cluster-gpu.md documents the exact failing command twice, calling agent 'the default since 20d3d14':

    outpost builtins set --cluster-mode=agent
    outpost restart   # restart picks up the new mode

So the documented way to select the cluster mode cannot work. Workaround used on novicortex 2026-07-22: set cluster.mode directly in agent.json and restart, which the daemon honors (logs 'mode=agent').

Fix: add ClusterMode to the MCP tool's input struct with the same jsonschema description as the CLI flag. Worth auditing the two structs for any other admincore param the MCP surface has drifted away from — a CLI flag that cannot reach the daemon is invisible until someone tries it.
