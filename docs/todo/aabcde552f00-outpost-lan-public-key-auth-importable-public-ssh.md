---
id: aabcde552f00
kind: feature
title: outpost LAN public-key auth + importable public SSH client package (prerequisite for bashy peer channel)
seq: 12
status: todo
priority: p0
labels:
    - remote
created: 2026-09-30T19:39:21.042906Z
sprint: 342
sprint_id: bdacb510-6448-5851-acf3-7a19f6ccccb2
sprint_title: 'bashy dag remote: run any target on another host as if it were local (self-bootstrap, sync, run, fetch back)'
---

Owner decision 2026-09-30 (sprint 342, option a): preserve the explicit install-time key-auth contract. Outpost must FIRST add (1) LAN public-key authentication to its in-process SSH server (outpost/internal/agent/ssh.go handleSSHConn/ServeLANListener: PublicKeyCallback against install-time authorized keys, alongside the existing OS-password gate; alternate port only, never disturb system sshd), and (2) an importable PUBLIC client package (outpost/internal/agent/sshclient is Go-internal and unreachable from bashy) exposing Dial/Exec/SFTP/DirectTCPIP/LocalForward semantics. Scope: outpost repo only; red/green tests; KISS. Coordinate with the outpost owner before editing (sprint 84 touched this area). Acceptance: bashy-side peer-channel story 5a917b8b can complete against it with key auth and no OS password/TTY. Blocks: 5a917b8b; downstream e7f7175b, 35386b1a e2e wait on the verified channel. Test constraint: alternate outpost port; do not stop system sshd; do not disturb novidesign.local user work.

RECONCILIATION 2026-09-30 with unsprinted todo:50e56e77492a (Sep 6, Sprint-96 context, broader pubkey design): ADOPT its decisions where overlapping — SameUser privilege guard (key must resolve to daemon OS user), key-file permissions/ownership rejection, ed25519+RSA, malformed-line ignore with logged reason, fingerprint-only logging (never key material or username paths), client -i ordered before password. NARROW 342 scope to the peer-channel need: LAN-listener pubkey + public client package + tests proving bashy-side unattended use. DEFER to 50e56e77492a's lane (do not duplicate): tunnel-entry parity, Windows coverage, key-management verbs, docs-wide trust-model updates. Read 50e56e77492a before designing; do not implement competing variants of what it specifies.
