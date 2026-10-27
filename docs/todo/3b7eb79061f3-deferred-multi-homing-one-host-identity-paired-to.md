---
id: 3b7eb79061f3
kind: feature
title: 'Deferred: multi-homing (one host identity paired to several servers at once)'
seq: 2729
status: todo
priority: p2
labels:
    - pairing
created: 2026-10-27T01:38:54.678049Z
---

One host key and owner, a list of pairings (server, scope, token, expiry). Pairing with a new server's code adds an entry instead of replacing; 'leave <server>' removes one; each pairing keeps its own tunnel and reconnect; a revoked or expired pairing drops only its own connection and is reported. Status lists all pairings. Existing single-pairing hosts migrate as a one-entry list. Red/green: fake two servers, revoke one, the other stays connected.
