# Matrix-shell — outpost-side handoff (May 2026)

Status: after the May 2026 fork sweep, the outpost-side follow-ups,
and the cloudbox peer-transport bypass landing end-of-round, the
matrix shell is at ~99% bash-equivalence for typical developer
workflows. `ssh`/`scp`/`sftp`/`ssh -L`/`ssh -R`/`ssh -A`/`ssh -J`/
`podman --connection` all work between paired outposts. The
remaining open items are well-scoped and independent — none block
typical use.

Companion to `matrix-shell-deferred-bugs.md`, which covers what was
investigated and closed.

## Shipped in this round (outpost side)

Listed newest first; all on `main`.

- **`97c5d40` `connect: add --ttl flag so outpost connect can request
  infinite cap`** — mirrors `outpost outbound add --ttl` for the SSH
  elevation path. Accepts `default | <duration> | infinite` via the
  shared `parseTTL` helper; when set, stamps `ttl_seconds` into the
  elevate POST body and cloudbox embeds it as the `mlt` claim on the
  elevation JWT. The idle TTL (1 h) still applies — pair with
  `--keep-alive` for an indefinitely-living session
  (`outpost connect --ttl infinite --keep-alive <host>`). Verified
  against ai.dhnt.io: `--ttl infinite` produces
  `mlt=9007199254740991` (2^53-1, the JS-safe sentinel); default
  omits `mlt` so older cloudboxes keep applying their 8 h cap.
- **`405c906` `ssh: stamp X-Outpost-Peer-Dial header on cloudbox-
  tunneled peer dials`** + cloudbox companion commit `33b619d`
  (`ssh: account-scoped peer-transport bypass for /h/:host/ssh`) —
  **T3.k now closed end-to-end.** Outpost stamps
  `X-Outpost-Peer-Dial: 1` + `X-Outpost-Peer-Origin: <self>` on its
  cloudbox-tunneled ProxyJump dial; cloudbox's new
  `MaybePeerTransport` middleware admits the dial when the bearer's
  email matches the destination's owner (per-bearer-sub rate limit:
  30/min, burst 10; every admit is audited). Crucially, the bypass
  does NOT stamp `X-Periscope-Role` — the destination outpost's own
  `PasswordCallback` still runs, so the operator gets a password
  prompt for the destination's OS user (matching vanilla openssh
  ProxyJump semantics). Verified live:
  `ssh -J host-b alice@host-c` works both directions.
- **`343a4f4` `cli: add outpost run --label X -- cmd to replace launchctl
  submit`** — T2.f workaround. `launchctl submit` is silently no-op'd
  inside the matrix-shell because the SSH session inherits a launchd
  system-domain context that doesn't have `submit` capability.
  `outpost run` generates a LaunchAgent plist, bootstraps it into
  `gui/<uid>`, persists it under `~/Library/LaunchAgents` so it
  auto-loads at next login. CLI: `outpost run --label X -- cmd`,
  `outpost run --list`, `outpost run --remove X`. Labels scoped under
  `outpost.run.<label>`. macOS-only — errors out cleanly elsewhere.
  Render gotcha: `html/template` escapes the leading `<?xml`, which
  launchd rejects with `Bootstrap failed: 5: Input/output error` —
  use `text/template` + `encoding/xml` escaping for user strings.
  Smoke-tested submit → ps → list → remove on host-a + host-c.
- **`aa54346` `ssh: add direct-streamlocal (podman ssh://) + peer-tunneled
  ProxyJump dial`** — two SSH server capabilities riding the existing
  /ssh WebSocket:
    - **direct-streamlocal@openssh.com**: podman's `ssh://<host>`
      transport opens this channel to forward to a remote unix socket.
      Allowlist built from `DetectPodman` + canonical docker sockets +
      operator-supplied `FileConfig.SSHForwardSockets`. See
      `docs/remote-podman.md`.
    - **peer-tunneled ProxyJump dial**: extends T3.k. The allowlist
      policy fix landed last round but the dial still fell through to
      LAN DNS. Now `handleDirectTCPIP` routes peer SSH dials
      (`ssh -J host-b host-c`) through cloudbox's
      `/h/<peer>/ssh` WSS endpoint with this outpost's own
      `access_token`. Loopback dials keep the zero-overhead path.
      Cloudbox originally 403'd on sibling-outpost tokens — see
      commits `405c906` + cloudbox `33b619d` above for the bypass
      that turns this into a working ProxyJump end-to-end.
  Refactored `sshHandler` to take an `sshHandlerDeps` struct while
  here — the positional arg list was at nine knobs.
- **`ssh: fix ssh -R back-channel, add exec-PTY, ssh -A, peer
  ProxyJump`** — four open items from the prior handoff become
  workable in one commit. See `docs/matrix-shell-deferred-bugs.md`
  for the per-item write-ups; the highlights:
    - **`ssh -R` (T1.a)**: `forwarded-tcpip` was carrying the
      canonicalized `"127.0.0.1"` as the back-channel destination
      address. OpenSSH's client looks up its remote-forward table by
      `strcmp` on the address it originally sent (typically `""`),
      so every back-channel was rejected with
      `unknown listen_port`. Echo the original `BindAddr`/`BindPort`.
    - **exec-with-PTY (T1.c)**: `ssh -tt host cmd` was returning
      "not a tty" because the exec branch ignored a preceding
      `pty-req`. New `outshell.Session.RunOnce` runs one command on
      the PTY-backed runner; the SSH exec branch routes through it
      when pty-req came first. Close-order matters here — close
      slave → drain master → close master, or the kernel buffer's
      tail bytes get dropped (cost me a deploy cycle before I caught
      it). Also dropped the spurious `clientGone` detection that
      tripped on openssh's immediate stdin-EOF.
    - **`ssh -A` (T1.b)**: new `internal/agent/sshagent.go` accepts
      `auth-agent-req@openssh.com`, sets up a per-session Unix
      socket in a 0700 tempdir, byte-bridges each accepted
      connection back via `auth-agent@openssh.com`. `SSH_AUTH_SOCK`
      is stamped into the runner env through the new
      `SessionOptions.Env` map. Gated by `SSHAllowAgentForward`
      (default on, mirrors the local/remote-forward toggles).
    - **ProxyJump allowlist (T3.k)**: new `peerhosts` package
      caches `/api/v1/ssh/hosts` (5-min TTL, serves stale on
      failure). The `direct-tcpip` allowlist now accepts any
      hostname that is itself a paired outpost, on top of loopback.
      The trust delegation is bounded — the destination's own
      OS-password gate still runs. Note: this is the
      policy-layer fix. The dial still uses LAN DNS, so it works
      end-to-end only when peers are mutually reachable (LAN /
      Tailscale / hairpin NAT). Cloudbox-tunneled peer dial is a
      separate feature.
- **`c35ea72` `shell: end session on exit builtin; stop killing it on
  bad commands`** — the interactive loop in
  `internal/agent/shell/runner.go` used to do `errors.As(err,
  &interp.ExitStatus)` to decide when the session was over, which
  matched *every* non-zero exit code — so `false`, a missing binary, a
  failing `grep`, etc. all killed the whole shell. Conversely the `exit`
  builtin with no args returned nil (exit code 0) and the loop ignored
  it, so typing `exit` was a no-op; users had to Ctrl-D out. Fixed by
  checking `runner.Exited()` (set only by the `exit` builtin or a fatal
  trap) for end-of-session, and silently swallowing plain `ExitStatus`
  errors — the command's own stderr already told the user what went
  wrong. Three regression tests in `runner_test.go` cover `exit`,
  `exit N`, and bad-command-survives. **No fork change needed**: the
  `exit` builtin in `interp/builtin.go` was already correct per its API
  contract; outpost was reading it wrong.
- **`d8201d2` `outbound: add outpost outbound CLI and per-mount TTL
  override`** — covered separately in the outbound docs; mentioned here
  only for completeness since it landed alongside the shell work.
- **`4cd8116` `sh: bump fork to b62bf936`** — pulls in the hint-string
  rename below.
- **`3032abf` `agent: persist detached bg jobs to outpost jobs/fg/bg/kill CLI`**
  — `NewSession` installs an `interp.WithBgPidCallback` that writes one
  JSON file per detached PID under `<UserCacheDir>/outpost/jobs/`. New
  CLI subcommands list / wait-on / signal those rows; `outpost bg` is a
  deliberate no-op surface (no SIGTSTP forwarding in this shell).
  Registry: `internal/agent/shell/bgjobs.go`. CLI: `cmd/outpost/jobs.go`.
- **`d59268d` `sh: bump fork to b40346f7`** — pulls in `WithBgPidCallback`.
- **`a955c78` `ssh: honor tcpip-forward (ssh -R) with loopback-only bind`**
  — `tcpip-forward` / `cancel-tcpip-forward` global requests are now
  handled; accepted connections push back as `forwarded-tcpip` channels.
  Per-conn `forwardRegistry` for teardown. Gated by
  `FileConfig.SSHAllowRemoteForward` (default on) and by
  `allowTCPIPForwardBind` (loopback only).
- **`e5ee46b` `ssh: propagate pty-req TERM into the shell runner`** —
  the in-process SSH session channel already opened a PTY pair via
  `outshell.NewSession()`; the gap was that `pty-req.Term` was captured
  but never plumbed into the runner env. `vim`/`htop`/`less` now see a
  real `TERM=xterm-256color` (or whatever the client requested) instead
  of the daemon's empty TERM. New `SessionOptions{Term, Cols, Rows}` +
  `BuildEnvWith()` overlay helper.

## Shipped in the fork (`external/sh`)

Two fresh commits on `master`, plus the three from earlier in the sweep
that the original handoff already named:

- **`b62bf936` `interp: name outpost jobs/fg/bg/kill in the job-control
  hints`** — `unsupportedHints["fg"/"bg"/"jobs"]` now point users at the
  external CLI by name, closing the loop the original commit
  `f38afec5` opened.
- **`b40346f7` `interp: add WithBgPidCallback`** — the embedder hook
  outpost wires into. Field is preserved across `Reset()` and
  `subshell()` so a running runner doesn't lose its callback on
  re-entry.
- `f38afec5` `interp: replace generic 'unsupported builtin' with actionable per-name hints`
- `db1fbd67` `interp: reject coproc cleanly instead of crashing`
- `88e0278d` `interp: add read -t TIMEOUT`

## Still open (outpost side)

### `screen -dmS` (T2.e)

**Still parked, with findings.** macOS's bundled `screen 4.00.03`
(from 2006) silently exits when invoked from the matrix-shell exec
environment — `screen -dmS x sleep 30` returns 0, no socket appears
under `$TMPDIR/.screen/`, no daemon process exists, no stderr emitted.
Same binary works fine in a regular login shell on the same host.
Tried (none helped): `nohup`, `setsid`, redirected stdio, custom
`SCREENDIR`, explicit `TERM`. Not a process-group / ctx-cancel issue
(the daemon never starts; nothing to kill). Without `dtruss` access
(requires SIP-off), narrowing further from outside is hard.

**Workaround:** `brew install screen` (modern build) or
`brew install tmux`. Both are expected to work.

### ControlMaster / ControlPersist (cloudbox-blocked)

**What's missing:** WSS connection reuse across multiple `ssh`
invocations. Tooling that fires many short SSH commands (Ansible, deploy
scripts) eats a 1-2s WSS handshake per call.

**Status:** needs a cloudbox-side protocol decision before outpost can
do anything. Out of scope until that's settled.

## Still open (fork side)

### Numbered-fd refactor

**What:** the fork's `interp/runner.go:1019` errors on any redirect
involving an fd outside `{0, 1, 2}`. So `exec 5>&1` doesn't work,
neither does `<&5`. This is what blocks real `coproc` support
(`coproc NAME { … }` exposes the child's pipes as
`${NAME[0]}`/`${NAME[1]}`, which are fd numbers users splice into
`<&N`/`>&N`).

**Why parked:** real implementation is 600-1000 LOC across the redirect
/ exec / pipeline paths and risks subtle existing-behavior breakage. The
silent-crash issue is already gone (`db1fbd67` prints a clean
rejection). File this as a "real coproc support" project for someone
who actually hits a coproc-shaped use case.

## Possible polish on what shipped

Small, non-blocking. Listed so it doesn't get lost.

- **Job-control CLI: capture command text.** The `WithBgPidCallback`
  signature is `(pid int)` only, so outpost records `"(detached)"` in
  the `Cmd` field. Extending the callback to also pass the statement
  text would let `outpost jobs` show what's actually running — needs
  fork-side stmt threading in `publishBgPid`, modest work.
- **Job-control CLI: PATH aliases.** Drop wrappers in `~/.local/bin/`
  for bare `jobs`/`fg`/`bg` (without the `outpost ` prefix) so
  muscle-memory commands work, per the original handoff sketch.
- **`pty-req` Modelist termios opcodes.** Currently ignored
  (`internal/agent/ssh.go:ptyReqMsg.Modelist`). Linux/macOS PTYs come up
  with sensible defaults so vim/htop work without these, but a
  finicky-termcap program might want them. One-line note in the struct
  doc explains the skip.
- **Admin UI toggle for `SSHAllowRemoteForward` / `SSHAllowAgentForward`.**
  Both fields are in `FileConfig` and threaded through to the handler,
  but the admin UI doesn't render checkboxes yet. JSON-only knobs for
  now; mirror the `SSHAllowLocalForward` UI when convenient.
- **Admin UI toggle for `SSHForwardSockets`.** JSON-only knob today;
  paired with the streamlocal allowlist that's now in `FileConfig`.
- **`outpost run` polish.** Currently `--list` only shows label +
  plist path. Adding a STATE column from `launchctl print
  gui/<uid>/<label>` parse would give the operator running/exited at a
  glance. Also `--stream LABEL` (tail StandardOutPath/StandardErrorPath)
  is the natural follow-up if operators actually use this verb.

## Suggested sequencing for what's left

1. **ControlMaster** — cloudbox-blocked; needs a shared protocol
   decision for WSS connection reuse across short SSH invocations.
2. **`screen -dmS` (T2.e)** — needs `dtruss` access (SIP-off) to
   diagnose further; workaround is `brew install screen` or tmux.
3. *(Indefinitely deferred)* numbered-fd refactor → real coproc.
