# Matrix-shell — deferred bugs

This is a scoping memo for three matrix-shell / outpost bugs that were
surfaced during a long multi-host automation run but **not** addressed
in the SFTP + builtins + keep-alive hardening pass. Each is parked here
with reproduction, suspected location, and verification notes so the
next agent can pick them up cleanly.

The matrix shell is an in-process `golang.org/x/crypto/ssh` server in
`internal/agent/ssh.go` that delegates command execution to a fork of
`mvdan.cc/sh/v3` (the qiangli/sh fork, vendored at `external/sh`). The
two shell-layer bugs below are in that fork; the launchctl bug is in
outpost's daemon-domain handling.

---

## #10 — Heredoc with nested f-string quotes corrupts silently

### Symptom

Bash heredocs that embed Python f-strings with nested `"…"` quotes get
silently mangled on the way through the matrix shell. Operators
working around it report having to switch to plain `print("k:", v)`
style (no f-string nesting), or to use `python3 << 'PY'` (the *quoted*
heredoc form) with explicit quote-form discipline.

### Reproduction skeleton

The submodule (master HEAD) already has test cases drafted for this — a
sibling agent began adding them. Check `external/sh/interp/interp_test.go`
around the existing heredoc tests for entries that look like:

```go
{
    "cat <<PY\nprint(f\"key: {v}\")\nPY",
    "print(f\"key: {v}\")\n",   // — expected; actual is mangled
},
```

If those aren't present, add a minimal case:

```bash
cat <<PY
print(f"key: {v}")
PY
```

Bash 5.x emits the body verbatim (parameter expansion happens for
unquoted heredocs, but `$v` isn't present here — the `"..."` should be
literal). Our parser appears to treat the double quotes as starting a
nested quoted region.

### Suspected location (per the exploration done while planning)

- **Parser**: `external/sh/syntax/parser.go:736-770` (`doHeredocs`) +
  the redirect collection at `:2063` (`p.heredocs = append(...)`).
- **Lexer**: `external/sh/syntax/lexer.go` heredoc body lexing — search
  for `hdocBody` / `hdocBodyTabs` quote state.

Bash reference semantics for **unquoted** heredocs (`<<EOF`):
- Parameter expansion (`$var`, `${var}`) happens
- Command substitution (`` `cmd` ``, `$(cmd)`) happens
- Arithmetic expansion (`$((expr))`) happens
- Backslash escapes: only `\$`, `\\`, `` \` ``, and `\<newline>`
- Everything else, including bare `"` and `'`, is literal

Bash reference semantics for **quoted** heredocs (`<<'EOF'` or `<<\EOF`):
- No expansion whatsoever; body is verbatim until the closing delimiter

The bug is almost certainly that the lexer's `hdocBody` quote state is
toggling on bare `"` instead of treating it as a literal byte.

### How to verify a fix

1. Add the repro test in `external/sh/interp/interp_test.go` `runTests`
   table.
2. Run `go test ./interp -run '^TestRunnerRun$' -count=1` — must pass.
3. Run the bash-oracle test: `CGO_ENABLED=0 go test -run TestRunnerRunConfirm
   -exec 'dockexec bash:5.2' ./interp` — must match real bash output
   for both quoted and unquoted heredoc variants of the same body.
4. End-to-end smoke through outpost: `outpost ssh-proxy somehost`,
   then paste the failing pattern, confirm clean output. Compare with
   stock ssh into a Linux box for the reference behavior.

---

## #11 — `$(...)` command substitution drops the result before argv split

### Symptom

```bash
ls -la $(which outpost)
```

returns the home-directory listing instead of `ls -la /path/to/outpost`.
The captured stdout (`/path/to/outpost\n`) isn't making it into argv as
`argv[1]` to the outer `ls -la`; instead `ls -la` runs with no path
argument and defaults to the cwd / home.

### Reproduction skeleton

```bash
echo "before: <$(printf abc)>"   # should print "before: <abc>"
ls -la $(printf '/etc/hosts')    # should list /etc/hosts metadata
set -- $(printf 'foo bar baz'); echo "got $# args: '$1' '$2' '$3'"
```

Note: the submodule already has draft test cases for this — check
`external/sh/interp/interp_test.go` near the existing `$(echo ...)`
tests for entries like:

```go
{`printf '<%s>\n' $(printf hello)`, "<hello>\n"},
{`printf '<%s>\n' $(printf 'a b')`, "<a>\n<b>\n"},
{`set -- $(printf 'foo bar'); echo $#:$1,$2`, "2:foo,bar\n"},
```

### Suspected location

- **Command substitution capture**: `external/sh/interp/runner.go:48-74`
  (`fillExpandConfig`) — sets up `cfg.CmdSubst` callback. The inner
  `stmts(ctx, cs.Stmts)` runs the subshell with stdout=captureBuf.
- **Substitution post-processing**: `external/sh/expand/expand.go:619-630`
  (`cmdSubst`) — trims trailing `\n`. Result returned as a single
  string.
- **Field splitting**: `external/sh/expand/expand.go:632-720`
  (`wordFields`) — supposed to split on IFS *after* substitution.

Hypothesis (to verify): the result of `$(cmd)` is being returned to the
caller but the field-splitting step that turns "the captured stdout"
into individual argv tokens isn't running for the OUTER command's argv.
Possibly conditional on context (inside `$(...)` itself works, but
substitution *as a positional arg* of another command doesn't).

### How to verify a fix

Same loop as #10:
1. Test cases in `interp_test.go` `runTests` pass.
2. Bash-oracle `TestRunnerRunConfirm` confirms parity with bash 5.2.
3. End-to-end smoke: `outpost ssh-proxy somehost`, run `ls -la
   $(which outpost)`, get the binary's listing (not home dir).

The fix probably ripples into a few existing tests that depended on
the broken behavior — that's worth a careful look.

---

## #8 — `launchctl submit` silently no-ops in the matrix-shell

### Symptom

```bash
launchctl submit -l my-label -- /path/to/program arg1 arg2
```

returns success (no error printed) but nothing actually launches —
`launchctl list | grep my-label` shows nothing. The same command run
from a normal Terminal session works fine.

### Root cause (hypothesis, not yet verified)

The SSH session inherits a launchd *domain context* from the outpost
process. Outpost typically runs as a LaunchAgent (user domain) or
LaunchDaemon (system domain). In either case, the inherited domain
doesn't have the `submit` capability — `launchctl submit` requires
either the GUI domain (`gui/$UID`) or an explicit bootstrap.

This isn't a shell bug. The shell faithfully ran what the user typed;
launchctl silently refused. There's no "Inappropriate ioctl" or
permission error because launchd considers this a no-op authorization,
not a failure.

### Workaround (works today)

Write a LaunchAgent plist and bootstrap into the user GUI domain:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key><string>com.example.my-label</string>
    <key>ProgramArguments</key>
    <array>
        <string>/path/to/program</string>
        <string>arg1</string>
        <string>arg2</string>
    </array>
    <key>RunAtLoad</key><true/>
    <key>KeepAlive</key><false/>
    <key>StandardOutPath</key><string>/tmp/my-label.out</string>
    <key>StandardErrorPath</key><string>/tmp/my-label.err</string>
</dict>
</plist>
```

Then:

```bash
launchctl bootstrap gui/$UID /tmp/my-label.plist
```

This survives every SSH session lifecycle because launchd owns it
under loginwindow's domain, not the SSH login session's.

### Possible fixes (none implemented)

1. **`outpost run --label X -- <cmd>` helper** — wrap the plist
   generation + bootstrap dance behind a single CLI verb, hiding the
   plist gore. The capability is already there; this is just a
   user-facing convenience.
2. **Re-bootstrap outpost into the GUI domain at install time** — so
   children of the outpost process can use `launchctl submit` directly.
   Invasive: changes the daemon's plist and may require user
   intervention on existing installs.
3. **Detect-and-warn in the shell** — have the matrix-shell intercept
   `launchctl submit` and print a hint to use the LaunchAgent
   workaround. Cheap and hostile to PowerUser preferences; probably
   not worth it.

The #1 option is the most natural; it lets agentic flows say
`outpost run --label kg3-pipeline -- ./kg3 serve` and forget about it.
No timeline — out of scope for the SFTP/builtins/keep-alive pass.

---

## Cross-cutting note

When picking up #10 or #11: the submodule may already have a sibling
agent's in-progress test scaffolding. Run `git status` inside
`external/sh/` before adding new test files to avoid conflict, and
coordinate with the other agent if you see uncommitted changes there.
