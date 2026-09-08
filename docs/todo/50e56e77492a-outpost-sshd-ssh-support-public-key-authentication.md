---
id: 50e56e77492a
kind: task
title: 'outpost sshd/ssh: support public-key authentication'
seq: 11
status: todo
priority: p1
created: 2026-09-06T21:25:20.156982Z
sprint: 96
---

Sprint 96 (Bashy Hop) names SSH as one of the two first-slice transport
adapters and requires that no hop forward a reusable credential by default.
Outpost's SSH surface cannot satisfy that today: it is password-only.

CURRENT STATE (verified in outpost@cafc092)

- Server: internal/agent/ssh.go builds ssh.ServerConfig with
  PasswordCallback + NoClientAuth only. There is no PublicKeyCallback and
  no authorized_keys anywhere in the tree. Both entry points share it:
  the tunnelled GET /ssh handler and the standalone LAN listener
  (cmd/outpost/sshd.go, ssh_listen_addr).
- The only passwordless paths are cloudbox vouching (X-Periscope-Role on
  a loopback bind) and the peer-ticket JWT. Both require pairing, so an
  unpaired/LAN-direct or automated hop must type an OS password.
- Client: cmd/outpost/ssh_runtime.go offers sshPasswordAuth() only —
  no identity file, no -i, no SSH_AUTH_SOCK consumption. (ssh -A agent
  FORWARDING exists, internal/agent/sshagent.go; that is the opposite
  direction and does not make the client key-capable.)

SCOPE

1. Server-side publickey auth on both entry points, sharing one
   authorizer so the tunnel and LAN listener cannot diverge:
   - PublicKeyCallback offered alongside PasswordCallback (password
     stays; this is additive, not a replacement).
   - authorized_keys source: standard per-user ~/.ssh/authorized_keys
     plus an outpost-owned file under the config dir. Decide and record
     which is authoritative and whether the OS file is opt-in.
   - Preserve the existing privilege guard: the authenticated identity
     must still resolve to the daemon's own OS user (hostauth.SameUser),
     because the session always runs as that account. A key must not
     become a way around that check.
   - Standard hygiene: reject bad permissions/ownership on the key file,
     support ed25519 + rsa-sha2, ignore malformed lines with a logged
     reason, honor MaxAuthTries, and log accept/reject with the key
     fingerprint (fingerprint only — never key material, never a path
     containing a real username).
2. Client-side publickey auth for `outpost ssh` / `outpost scp`:
   -i/--identity, default identity discovery, SSH_AUTH_SOCK when
   present, ordered before password so an unattended hop never blocks
   on a prompt; password remains the fallback.
3. Key management verbs sufficient to install a key without a shell on
   the far side (e.g. an outpost-native ssh-copy-id equivalent, or an
   admin API/CLI to add/list/remove authorized keys). Enrolling a key
   must itself be authenticated by an existing path.
4. Docs: cmd/outpost/sshd.go long help, outpost/CLAUDE.md, and the
   trust-model comment block at the top of internal/agent/ssh.go all
   currently state "the OS password is the gate" — update all three.

ACCEPTANCE

- `ssh -i key -p 2222 <os-user>@<host>` and `scp -i key` succeed against
  `outpost sshd` on an UNPAIRED host with no password, using both system
  ssh and `outpost ssh`, on macOS + Linux + Windows (per-OS runtime
  test, not cross-compile).
- The same key works through the tunnelled /ssh path.
- Negative gates, each with a test: unknown key rejected; known key for
  a DIFFERENT OS account rejected (SameUser guard holds); world-writable
  authorized_keys rejected; revoked key stops working immediately.
- Password auth still works unchanged, and cloudbox vouching /
  peer-ticket paths are untouched.
- go test ./... green, plus the per-OS runtime check above.

NOTES / TRAPS

- Server and client are separable; the server half is what unblocks a
  Hop SSH adapter, so land it first.
- Do not weaken NoClientAuth or the vouched paths while adding this.
- Sprint 96 is parked behind Sprint 88; this story is filed for planning
  and must not start implementation until Bashy Hop is opened.
