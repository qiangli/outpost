---
id: 415e67d603d3
kind: task
title: 'outpost peers list: CLI/daemon schema mismatch'
seq: 3
status: todo
priority: p2
created: 2026-07-22T06:12:48.281716Z
---

`outpost peers list` fails against a populated discovery cache:

  Error: json: cannot unmarshal object into Go value of type []discovery.Peer

The CLI decodes into a slice; the daemon returns an object. Reproduced on novicortex (v0.14.2) with peers present. On a host with an EMPTY cache the same command succeeds and prints 'Discovery cache is empty', so the bug only shows once there is something to list — which is why it can sit unnoticed.

Fix one side to match the other and add a decode test with a non-empty cache; an empty-cache test would pass either way and give false confidence.
