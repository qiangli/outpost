---
id: 4644db011886
kind: feature
title: Export outpost in-process LAN SSH server as public pkg/sshserver for bashy peer channel
seq: 13
status: done
priority: p0
labels:
    - remote
created: 2026-09-30T21:07:07.994174Z
weave: 4
assignee: codex-gpt5.6-luna
sprint: 342
sprint_id: bdacb510-6448-5851-acf3-7a19f6ccccb2
sprint_title: 'bashy dag remote: run any target on another host as if it were local (self-bootstrap, sync, run, fetch back)'
closed: 2026-09-30T21:24:12.616326Z
closed_by: codex-gpt6-sol
---

Prerequisite for bashy story 5a917b8bc4d3. Existing outpost/internal/agent.ServeLANSSH and cmd/outpost/sshd.go are internal to outpost and cannot be imported by bashy. Provide minimal public pkg/sshserver API that accepts a context, listener/address, install-time authorized_keys path and host-key identity, serves exec/SFTP/direct-tcpip forwarding with SameUser/auth policy, and shuts down cleanly. Reuse existing implementation; no protocol copy. Red/green loopback key-auth exec, file transfer and forwarded port tests. Gate: go test ./pkg/sshserver/... ./internal/agent/... ./cmd/outpost/... -run "SSH|LAN" -count=1. Link before peer channel story; no system sshd or external outpost process after bootstrap.
