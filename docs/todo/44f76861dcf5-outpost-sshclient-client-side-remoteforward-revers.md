---
id: 44f76861dcf5
kind: feature
title: 'outpost sshclient: client-side RemoteForward (reverse tcpip-forward) for peer-channel egress'
seq: 15
status: done
priority: p1
labels:
    - remote
created: 2026-10-06T01:30:02.899822Z
weave: 1
assignee: codex-gpt5.6-sol
sprint: 343
sprint_id: ed2cbe05-a807-56d6-81dc-a9a04e7a11f5
sprint_title: 'Built-in tunnelling for air-gapped remotes: SOCKS5 and HTTP/HTTPS proxy over the bashy peer channel'
closed: 2026-10-06T02:13:02.98021Z
closed_by: claude-fable5.1
---

Sprint 343 prerequisite for the reverse-proxy story (bashy 29b9661d). Add a client-side remote port-forward to outpost/pkg/sshclient: a RemoteForward method that sends an SSH tcpip-forward global request for a loopback bind address, accepts the resulting forwarded-tcpip channels, and dials each to a caller-supplied local target (the operator loopback proxy address), with context cancellation and clean teardown via cancel-tcpip-forward. The SERVER side already exists in outpost/internal/agent/ssh.go (handleTCPIPForward, gated by AllowRemoteForward, loopback only) and this adds only the missing CLIENT half beside the existing Exec, SFTP, DirectTCPIP and LocalForward. Keep it pure Go on golang.org/x/crypto/ssh. Acceptance: red/green test in pkg/sshclient that stands up the in-process server with AllowRemoteForward enabled, requests a remote forward, connects to the remote bind address and verifies bytes round-trip to a local echo target, plus a cancellation test. No change to the wire protocol or the reserved namespace.
