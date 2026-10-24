---
id: 9c331c064ea6
kind: bug
title: ssh over the tunnel hangs when an older outpost client (77f7547) talks to a v1.0.0-rc.12-dev outpost server; elevation succeeds but exec never returns
seq: 1902
status: assigned
priority: p1
created: 2026-10-23T20:17:46.771843Z
weave: 82
assignee: muse-conductor
sprint: 497
sprint_id: 16af0380-7ec7-5d41-a9c6-50e1b03cdf11
sprint_title: 'Fix the 2026-10-23 findings: satellite login/superadmin security, web console multi-satellite UX, fleet downgrade trap, Windows agent bugs, weave/sprint tooling, outpost headless/version skew'
---

Found 2026-10-23 during the lern->ivy migration. The client is the macOS outpost 77f7547 (built 2026-10-19). Against a host running v0.32.0-dev, 'outpost ssh user@host cmd' works; against the same host after updating to v1.0.0-rc.12-dev it hangs (timeout), while 'outpost connect' elevation succeeds. A client built from outpost main (3c7ee41) works. Users with older clients lose ssh/exec to updated hosts. Expected: wire compatibility across one release, or a clear version-skew error instead of a hang.

## Worker brief (s497-manager, 2026-10-23). One repo: outpost. Investigation + fix.

Survey: client cmd/outpost/ssh_runtime.go `dialPairedHost:129` tries elevate, then dialLANDirect:343 (mDNS lan-ssh-ws + peer ticket), then dialMeshDirect:225 (libp2p forward), then dialCloudboxTunnel:412. Server internal/agent/ssh.go (sshHandler:117, handleSSHConn:233, ServeLANSSHWS:436, handleSSHSession:950, runExecCommandPTY:1202). Changes between the old client 77f7547 (2026-10-19) and current main touching the wire: 82882ff (hashicorp/yamux -> qiangli/yamux v0.1.0 on the tunnel), 56ddb9c (libp2p/go-yamux/v5 -> qiangli/yamux/v5 on the mesh), 9cf6644 (sshd unit, ssh_listen_addr removed, mDNS advertising changed). Elevation works, only exec hangs -> a transport rung.
Do: (1) reproduce locally with two builds: `git worktree add` 77f7547 as the client and HEAD as the server, in-process/loopback (use existing harnesses: cmd/outpost/ssh_proxy_test.go, ssh_runtime_test.go, sshd_e2e_test.go, internal/agent/mesh/forward_test.go, yamux/interop/v5_matrix_test.go). Bisect rung by rung (mesh, tunnel, LAN). (2) Fix so an older client either works or gets a clear version-skew error, and (3) add a hard per-rung dial/first-byte timeout so a hanging rung falls through to the next (and finally errors) instead of hanging forever. Red/green test for the rung timeout and, if feasible, an interop test for the incompatible rung.
Constraints: tests must use isolated HOME/XDG dirs and loopback only; never touch the real paired config, launchd agent or real hosts. Do not exec the test binary itself.
Gate: `go test -short` on touched packages (cmd/outpost needs -short; internal/agent/shell needs a TTY: skip it) + `go build ./...` for linux/darwin/windows. Write findings in the commit body. Trailers for this story only. No hostnames.
