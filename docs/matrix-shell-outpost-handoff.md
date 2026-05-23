# Matrix-shell — outpost-side handoff (May 2026)

Status: after the May 2026 fork sweep and the outpost-side follow-up
landed in this round, the matrix shell is at ~99% bash-equivalence for
typical developer workflows. The remaining open items are well-scoped
and independent — none block typical use.

Companion to `matrix-shell-deferred-bugs.md`, which covers what was
investigated and closed.

## Shipped in this round (outpost side)

Listed newest first; all on `main`.

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

### `ssh -A` — agent forwarding

**What's missing:** the SSH server doesn't accept
`auth-agent-req@openssh.com` channels. Every `git pull` / `git push` on
a paired host has to re-auth (typically by re-typing a passphrase)
instead of riding the operator's local ssh-agent.

**Roughly:** accept the channel request, dial the host's
`$SSH_AUTH_SOCK`, byte-bridge the two sides. The auth-agent protocol is
opaque to the forwarder — same shape as the existing `direct-tcpip`
handler.

### ProxyJump allowlist (Tier 3)

**Where:** `internal/agent/ssh.go`'s `direct-tcpip` handler
(`allowDirectTCPIPDest`).

**What's wrong:** the destination allowlist is `{localhost, 127.0.0.1, ::1}`.
`ssh -J novicortex noviadmin@novidesign` fails because the second hop's
destination (`novidesign`) isn't loopback.

**Fix policy options:**

1. Widen to any hostname the agent can resolve (matches OpenSSH default;
   broadest trust).
2. Add a registry-based allowlist — "any host that's also a paired
   outpost" — via a cloudbox lookup. Tighter, but needs cloudbox
   coordination on the registry-query API.
3. Operator-side workaround stays: two-hop manually. Not actually a fix.

Pick a policy, implement the check. Pure outpost-side change once the
policy is decided.

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
- **Admin UI toggle for `SSHAllowRemoteForward`.** The field is in
  `FileConfig` and threaded through to the handler, but the admin UI
  doesn't render a checkbox yet. JSON-only knob for now; mirror the
  `SSHAllowLocalForward` UI when convenient.

## Suggested sequencing for what's left

1. **`ssh -A` agent forwarding** — small change, big git-workflow win.
2. **ProxyJump allowlist** — tiny once the policy is decided.
3. **ControlMaster** — gated on cloudbox.
4. *(Indefinitely deferred)* numbered-fd refactor → real coproc.
