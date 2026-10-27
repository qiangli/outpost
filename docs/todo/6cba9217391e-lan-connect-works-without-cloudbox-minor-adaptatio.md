---
id: 6cba9217391e
kind: feature
title: 'LAN connect works without cloudbox: minor adaptation of existing outpost sshd/ssh (local first, MVP)'
seq: 2757
status: todo
priority: p1
labels:
    - ssh
    - outpost
    - lan
created: 2026-10-25T13:12:04.639359Z
assignee: codex-gpt6-astra
sprint: 505
sprint_id: 4354bb44-85fc-5fff-b997-58e305948c09
sprint_title: 'v1.0 user-path gaps: master lists every host a user owns across satellites; a release reaches every host and satellite'
---

MVP/KISS (operator 2026-10-25): the connection should already mostly work; this story is a minor adaptation so the LAN/p2p path also works when cloudbox/satellite is down (local first). No new subsystem, no new pairing protocol.

Already exists (verify, do not rebuild): outpost sshd :2222 with authorized_keys key auth + boot service (8ddd0428), mDNS lan-ssh advert, `outpost ssh` LAN-direct fallback when the cloudbox flow fails, `bashy sshd install --authorize <pubkey>`, tunneled ssh via cloudbox.

Steps:
1. Red first: from dev-box, with cloudbox unreachable, try `bashy connect <lan host> -- hostname` (key auth, no password). Record exactly what fails.
2. Fix only those gaps, smallest change. Expected candidates: connect/outpost ssh should try LAN-direct + key auth first (not after a cloudbox timeout); sshd must not need cloudbox to start. Anything bigger is out of scope: no follow-up stories or todos.
3. Keys: reuse `bashy sshd install --authorize`; no automatic key sync, no code pairing in this story.

Acceptance:
- cloudbox down: `bashy connect host-e -- hostname` (or another LAN host) from dev-box succeeds, no password.
- cloudbox up: the tunneled path still works (no regression).
- Never trust a host because it answered on the LAN: key auth only, host key pinned.

RED (s505-manager, 2026-10-25 18:50Z, dev-box -> otter, bashy only):
- `bashy outpost ssh user@otter 'outpost version'`: mesh-direct fails (cloudbox 401 "invalid elevation cookie: token signature is invalid"), cloudbox-assisted dial fails, then LAN-direct asks for a PASSWORD ("no TTY for password prompt").
- `bashy outpost ssh user@otter.local:2222 ...` (explicit LAN, no cloudbox): same, password only.
- Root cause: the outpost ssh CLIENT's LAN-direct paths offer only password auth (outpost cmd/outpost/ssh_runtime.go sshPasswordAuth ~L679). The server already accepts keys (`outpost ssh authorize add`, outpost.sshd unit authorized_keys); dev-box's ~/.ssh/id_ed25519.pub is now authorized on otter (<fingerprint>).

FIX (one repo: outpost; smallest change):
- On LAN-direct and explicit host:port dials, offer public-key auth first: keys from SSH_AUTH_SOCK agent if set, else ~/.ssh/id_ed25519, id_ecdsa, id_rsa (unencrypted; skip encrypted quietly). Password (TTY or OUTPOST_SSH_PASSWORD) stays as the fallback after keys.
- Pin/verify host key as today (no trust-by-LAN).
- Tests: in-process sshd test that accepts only a key: key-only login succeeds with no TTY and no OUTPOST_SSH_PASSWORD; wrong key + no password fails cleanly; password fallback unchanged. Gate: go test -short ./cmd/outpost/... + go build ./... + GOOS=windows go build ./cmd/outpost. No yoke/cloudbox changes; no new flags.
- Live (conductor): `bashy outpost ssh user@otter.local:2222 -- hostname` from dev-box, no password, cloudbox blocked.

DONE part 1: outpost 72c776e (main) — LAN-direct dials try keys (agent, else id_ed25519/id_ecdsa/id_rsa) before password. Tests + build + windows build green.

LIVE rollback test on otter (s505-manager, 2026-10-25, rc.15 = outpost 74ae98e):
- exit-at-boot candidate: swap 19:12:13 -> auto_rollback 19:12:32 (boot_count=3), quarantined, reachable 19:12:50. PASS.
- hanging candidate (stays up, never confirms): swap 19:14:04; confirm deadline 19:24:04 passed with NO revert (hung daemon never respawns, watchdog only runs at respawn); host unreachable until the scheduled reboot 19:28:18; reverted 19:28:39 (deadline_passed=true). FAIL for headless hosts.

REWORK (one run, outpost only, smallest change):
1. sshDirectAuth: if SSH_AUTH_SOCK is set but the agent is unreachable or has zero signers, still try the id_* files (macOS always sets SSH_AUTH_SOCK). Test: agent set but empty -> file key used.
2. Watchdog deadline while the daemon is UP: the supervisor also checks the pending-confirm marker on a timer (e.g. every 30s) while the daemon runs; once confirm_deadline has passed unconfirmed, stop the daemon, revert to .previous, quarantine, restart (same code path as the respawn revert; ledger step auto_rollback). Test with a fake daemon that hangs: reverted shortly after the deadline without a restart or reboot.
Gate: go test -short ./cmd/outpost/... ./internal/agent/upgrade/... + go build ./... + GOOS=windows go build ./cmd/outpost; umbrella pin bump for outpost.
Conductor re-runs the hanging-candidate test live on otter after release.

Status 2026-10-26 (s505-manager):
- Rework run dhnt#180 (codex-gpt6-astra) is SUBMITTED, NOT YET REVIEWED: outpost c1ada25 "recover hung upgrades and fall back from empty SSH agents" (supervisor deadline timer + agent-empty -> id_* files) + umbrella pin f00cc216b. Land it only through the worker's own commit (weave pull/salvage), never a manager cherry-pick.
- ADD to this story (in scope: "host key pinned, no trust-by-LAN"): the outpost ssh client re-pins a CHANGED host key by default (warn only, unless OUTPOST_SSH_STRICT_HOST_KEY=1). Seen live 2026-10-25: lanhost.local :22 (OpenSSH) and :2222 (outpost sshd) share the alias outpost-lanhost.local, so every alternate dial "changes" the key. Fix: strict by default for direct dials; key pins per host:port so 22 and 2222 do not collide.
- Then: release, re-run the hanging-candidate test on otter (expect revert shortly after the confirm deadline with no reboot), close.

Status 2026-10-26 later (s505-manager):
- Run dhnt#180 was abandoned before its outpost commit was preserved; the workspace (and c1ada25) is gone. Redo in a new run, outpost repo, same scope (REWORK 1+2 above) PLUS the host-key item: strict by default for direct dials, pins keyed per host:port.
- Recovered partial patch of the supervisor/watchdog half (from the worker transcript) is provided in the brief; ssh_runtime + tests must be rewritten.

BRIEF for the redo run (outpost only):
1. sshDirectAuth: SSH_AUTH_SOCK set but agent unreachable or zero signers -> still try ~/.ssh/id_ed25519, id_ecdsa, id_rsa. Test: agent empty -> file key used.
2. Supervisor: `Program.RestartWhen func() bool` + `RestartCheckInterval` (default 30s). `waitForChild` polls it while the child runs, stops the child via a per-child context, then the normal PreStart revert path runs. supervisord wires `RestartWhen` to `watchdogDeadline(confirmPath)` (upgrade.ReadPendingConfirm; true once now > ConfirmDeadline). Test with a fake hanging daemon: reverted shortly after the deadline with no restart or reboot. A recovered partial patch of this half from the previous attempt is at `$HOME/.bashy/s505-6cba9217-recovered-watchdog.patch` (apply_patch format) — reuse it.
3. Host keys: direct dials are STRICT by default (a changed key is refused with a clear message; `OUTPOST_SSH_STRICT_HOST_KEY=0` opts out); pins are keyed per host:port so :22 and :2222 never collide. Tests for both.
Gate: `go test -short ./cmd/outpost/... ./internal/agent/supervisor/... ./internal/agent/upgrade/...` && `go build ./...` && `GOOS=windows go build ./cmd/outpost`. Commit in outpost with trailers (Sprint: #505, Story: #2757, Story-ID: 6cba9217391e). No hostnames in code, tests or docs.
