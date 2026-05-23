# Matrix-shell — outpost-side handoff (after May 2026 fork sweep)

Status: the matrix shell is at ~97% bash-equivalence for typical developer
workflows. After the May 2026 sweep of `external/sh` (the qiangli/sh fork),
the remaining gaps live on **the outpost side** — the SSH server, the PTY
allocation path, and a missing external job-control CLI. This doc inventories
what's left so the work doesn't drift.

Companion to `matrix-shell-deferred-bugs.md`, which covers what was
investigated and closed.

## What just shipped in the fork

For context, three improvements landed in `external/sh` and are already
deployed via the vendored module:

- **`88e0278d` `interp: add read -t TIMEOUT`** — `read -t` now parses
  fractional seconds, wraps the underlying read in `context.WithTimeout`,
  exits 142 on deadline, and rejects negative values. Closes the Tier 3
  smoke-test finding that `read: invalid option "-t"` blocked common
  patterns.
- **`db1fbd67` `interp: reject coproc cleanly instead of crashing`** —
  bash `coproc` parsed successfully but ran into "unhandled command node"
  at runtime, which was effectively a silent crash. It now prints a clear
  "not supported in this shell — use a fifo (mkfifo) with a background
  command, or process substitution" message. Real coproc support remains
  deferred (see § "Deferred — numbered-fd refactor" below).
- **`f38afec5` `interp: replace generic 'unsupported builtin' with
  actionable per-name hints`** — 17 bash/POSIX builtins (`fg`, `bg`,
  `jobs`, `umask`, `ulimit`, `history`, …) that the runner recognized
  but did not implement used to fall through to a single uninformative
  "<name>: unsupported builtin" line. Each now points at a workaround
  through a hint table in `interp/builtin.go`. **This is the line that
  promises the existence of an external job-control command** — see
  § "External job-control CLI" below for the matching commitment.

The rest of this doc is what outpost owes back.

## Tier 1 — SSH server gaps

These all live in `internal/agent/ssh.go` and are the bulk of remaining
matrix-shell parity work. Listed in roughly increasing operational pain.

### `ssh -t` — real PTY allocation

**What's missing:** the in-process `golang.org/x/crypto/ssh` server in
`internal/agent/ssh.go` doesn't allocate a `pty` for the session channel;
the runner's stdin/stdout/stderr are wired directly to the WebSocket conn.
Without a PTY, terminal-aware programs (`vim`, `nvim`, `htop`, `less` in
interactive mode, `mc`, tab-completion, anything that calls `tcsetattr`)
either refuse to start or behave oddly.

**Prerequisite for** any future `Ctrl-Z`/`SIGTSTP` work in this shell —
without a PTY there's literally no signal surface for the operator to type
into.

**Roughly:** accept the `pty-req` request in the session channel handler,
allocate a pty pair (the existing `internal/agent/shell/pty_*.go` paths
already do this for the `/shell` WS route — likely reusable), set the
runner's stdio to the slave side, propagate window-size updates via
`window-change` requests.

### `ssh -A` — agent forwarding

**What's missing:** the SSH server doesn't accept
`auth-agent-req@openssh.com` channels. Every `git pull` / `git push` on a
paired host has to re-auth (typically by re-typing a passphrase) instead
of riding the operator's local ssh-agent.

**Roughly:** accept the channel request, dial the host's
`$SSH_AUTH_SOCK`, byte-bridge the two sides. The auth-agent protocol is
opaque to the forwarder — same shape as the existing `direct-tcpip`
handler.

### `ssh -R` — reverse port forward

**What's missing:** the `tcpip-forward` global request handler. Without
it, you can't expose a service on your laptop to a paired host — which
blocks "paired host pulls artifacts off my laptop" and "ssh from the
paired host into a local dev server" workflows.

**Roughly:** mirror image of the existing `direct-tcpip` handler. Bind
a listener at the requested address/port on the agent side, accept
incoming TCP conns, multiplex them back to the client as
`forwarded-tcpip` channels. Lifecycle is the fiddly part — the listener
has to be torn down when the SSH session ends or when a matching
`cancel-tcpip-forward` arrives.

### ControlMaster / ControlPersist

**What's missing:** WSS connection reuse across multiple `ssh` invocations.
Tooling that fires many short SSH commands (Ansible, deploy scripts) eats a
1-2s WSS handshake per call.

**Status:** needs a cloudbox-side protocol decision before outpost can do
anything. Out of scope until that's settled.

## Tier 3 — ProxyJump allowlist

**Where:** `internal/agent/ssh.go`'s `direct-tcpip` handler.

**What's wrong:** the destination allowlist is currently
`{localhost, 127.0.0.1, ::1}`. `ssh -J novicortex noviadmin@novidesign`
fails because the second hop's destination (`novidesign`) isn't loopback.

**Fix policy options:**

1. Widen to any hostname the agent can resolve (matches OpenSSH default;
   broadest trust).
2. Add a registry-based allowlist — "any host that's also a paired
   outpost" — via a cloudbox lookup. Tighter, but needs cloudbox
   coordination on the registry-query API.
3. Operator-side workaround stays: two-hop manually. Not actually a fix.

Pick a policy, implement the check. Pure outpost-side change once the
policy is decided.

## External job-control CLI

This is the commitment the fork's hint messages now make. `fg`, `bg`, and
`jobs` in the matrix shell currently say *"not supported in this shell —
use an external job-control command"*. That command needs to exist.

### Surface

Add the following as subcommands of `cmd/outpost/`:

- `outpost jobs` — list known background jobs the operator has launched on
  this host.
- `outpost fg JOBID` — bring a backgrounded job to the foreground (i.e.
  `wait` on it and stream its stdio, if still attached).
- `outpost bg JOBID` — resume a suspended job. Whether suspension is
  meaningful depends on whether we ship Ctrl-Z forwarding to real
  subprocesses; until then `bg` is mostly a no-op-with-message for
  symmetry.
- `outpost kill JOBID [SIGNAL]` — signal-deliver to a recorded job.

Optionally PATH-alias each under the bare names `jobs`/`fg`/`bg` (a
symlink farm or a wrapper script the installer drops in
`~/.local/bin/`) so muscle-memory commands work without the `outpost`
prefix.

### Backing store

Persistent job registry at `<UserCacheDir>/outpost/jobs/` — either a
sqlite file or a directory of JSON records, doesn't matter much.

The registry is the **source of truth**. The fork no longer has a `jobs`
builtin to disagree with it. That's deliberate: with goroutine-subshells,
the fork couldn't ship POSIX-correct job control anyway, so we pushed the
state out to a process that *can* use real OS primitives.

### Integration hook

When a user runs `nohup foo &` or `setsid foo &` in the matrix shell, the
resulting detached process needs to land in the registry. The integration
point lives in the fork at `interp/api.go:281-290` (`publishBgPid`) — the
runner calls this whenever a backgrounded statement spawned a real `exec`
and the kernel handed out a PID.

Outpost wires this up by installing a callback at runner construction
time. Sketch:

```go
// in internal/agent/shell/runner.go (wherever you build the interp.Runner)
runner, err := interp.New(
    interp.StdIO(...),
    interp.Env(...),
    // ... existing options ...
    interp.WithBgPidCallback(func(pid int, cmd string, ts time.Time) {
        registry.Insert(jobRecord{
            PID: pid,
            User: currentUser,
            Cmd: cmd,
            StartedAt: ts,
        })
    }),
)
```

(The fork doesn't actually expose a `WithBgPidCallback` option yet — that
hook surface needs to be added in `external/sh` as part of this work. Or
outpost can poll `r.bgProcs` via a custom `ExecHandler` middleware that
intercepts each exec. Either is reasonable; the callback is cleaner.)

### Naming the external tool in the fork's hints

Once the external CLI's name is stable, update the fork's
`unsupportedHints` map (`external/sh/interp/builtin.go`) to reference it
explicitly. Today the hints say "an external job-control command"
intentionally — that phrasing is generic because the fork is upstreamable
to non-outpost embedders. When outpost ships its own pinned vendored
copy, we can replace those strings with `outpost jobs` etc. in the
qiangli/sh fork without affecting upstream merge.

## Deferred — numbered-fd refactor in the fork

For completeness, this is the only remaining item that lives *back* in
the fork rather than in outpost. It's parked, not active.

**What:** the fork's `interp/runner.go:1019` errors on any redirect
involving an fd outside `{0, 1, 2}`. So `exec 5>&1` doesn't work, neither
does `<&5`. This is what blocks real `coproc` support (`coproc NAME { … }`
exposes the child's pipes as `${NAME[0]}`/`${NAME[1]}`, which are fd
numbers users splice into `<&N`/`>&N`).

**Why parked:** real implementation is 600-1000 LOC across the redirect /
exec / pipeline paths and risks subtle existing-behavior breakage. The
silent-crash issue is already gone (commit `db1fbd67` in the fork
prints a clean rejection). File this as a "real coproc support" project
for someone who actually hits a coproc-shaped use case.

## Suggested sequencing

Independent enough that two people can work in parallel — but if I were
sequencing for one developer:

1. **`ssh -t` PTY first.** Unlocks `vim`, `nvim`, `htop`, `less`,
   tab-completion — by far the biggest QoL win.
2. **`ssh -A` agent forwarding.** Small change relative to PTY, big
   git-workflow win.
3. **ProxyJump allowlist.** Tiny; gated only on the policy decision.
4. **External `outpost jobs` CLI.** Independent of the SSH items;
   ideal for a second person in parallel.
5. **`ssh -R`.** Useful but the most niche of the Tier 1 set.
6. **ControlMaster.** Last, gated on cloudbox.
7. *(Deferred indefinitely)* numbered-fd refactor in the fork → real coproc.
