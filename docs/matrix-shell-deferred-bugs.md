# Matrix-shell — deferred bugs

Status after the second hardening pass (May 2026):

- **#11 (`$(which outpost)` returns empty)** — **fixed**. Was launchd
  PATH narrowing, not an interpreter bug. See `internal/agent/shell/env.go`
  for the fix (BuildEnv prepends outpost's own exe dir + common user dirs)
  and `interp/interp_test.go` for the fork-side regression tests proving
  the interpreter itself was always correct.
- **#10 (heredoc curly-quote corruption)** — **not reproducible**. The
  fork interpreter handles Unicode curly quotes (`"`/`"`, U+201C/U+201D)
  byte-for-byte in heredoc bodies; verified end-to-end through
  `outpost ssh-proxy` (cloudbox + matrix tunnel + matrix shell). If the
  bug exists, it's almost certainly in xterm.js / the browser `/shell`
  PTY terminal frontend, not in outpost. Sibling-fork test cases at
  `interp/interp_test.go:1543-1554` are kept as permanent guards.
- **#8 (`launchctl submit`)** — still parked. macOS launchd domain issue,
  not addressable from the shell layer. Workaround unchanged.

Each is detailed below.

The matrix shell is an in-process `golang.org/x/crypto/ssh` server in
`internal/agent/ssh.go` that delegates command execution to a fork of
`mvdan.cc/sh/v3` (the qiangli/sh fork, vendored at `external/sh`).

---

## #10 — Heredoc with nested f-string quotes (NOT REPRODUCIBLE)

### Status

The other agent diagnosed the fork interpreter: `quotedHdocWord`
(`syntax/lexer.go:1212`) and `hdocReader` (`interp/runner.go:920`) both
pass bytes through unchanged. Regression tests at
`interp/interp_test.go:1543-1554` (commit `e552dd40` upstream) prove
this for both quoted (`<<'PY'`) and unquoted (`<<PY`) heredocs with
Unicode curly quotes inside.

End-to-end smoke confirmed it through the SSH exec path:

```bash
$ ssh novidesign 'cat <<PY
> he said "smart" quotes
> print(f"key: {v}")
> data = {"a": 1, "b": 2}
> PY'
he said "smart" quotes
print(f"key: {v}")
data = {"a": 1, "b": 2}
```

Bytes survive cleanly through `dragon ssh-proxy → cloudbox → matrix
tunnel → novidesign outpost → ssh server exec channel → qiangli/sh
runner`. Both Unicode curly quotes and ASCII double quotes pass through.

### Where to look if it resurfaces

- **Browser `/shell` PTY-WebSocket bridge** (`internal/agent/shell.go`)
  is the remaining suspect. The bridge reads 4096-byte chunks from the
  PTY master and emits a binary WS frame per `Read()`. If xterm.js's
  `TextDecoder` is invoked per-frame (not in streaming mode), a
  multi-byte UTF-8 sequence split across a frame boundary would render
  as `U+FFFD` replacement chars. Output direction only; input direction
  (browser → WS → PTY) is byte-stream all the way down so unaffected.
- The user's original report mentioned "SSH wire layer," but smoke shows
  the SSH path is byte-clean. They may have been using the browser
  shell at the time and attributed it loosely; the description was
  written in a hurry between automation runs.

### How to verify a fix (if the browser shell ever ends up under
investigation)

1. Drive the browser shell with a heredoc containing curly quotes
   AND a longer-than-4 KiB body — increases the likelihood of a
   chunk-boundary split landing mid-rune.
2. Inspect WS frames in browser devtools; confirm whether
   individual frames end mid-rune.
3. Fix: feed PTY output into a `bufio.Writer` or a small
   "rune-aligned chunker" that holds back trailing partial-runes
   until the next byte arrives. Cost is one buffer + a tiny
   `utf8.RuneStart`-style scan.

---

## #11 — `$(which outpost)` empty in argv (FIXED)

### Root cause

The fork interpreter was always correct — the other agent confirmed
`$(cmd) → argv` works for arbitrary substitutions
(`interp/interp_test.go:350-352`). The actual bug was on outpost's side:
the matrix shell ran with the launchd-spawned daemon's process env,
which on macOS LaunchDaemons is `/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:/opt/homebrew/bin`
— missing `/Users/<user>/bin` where the outpost binary itself usually
lives. So `which outpost` returned empty, `ls -la $(which outpost)`
became `ls -la` (cwd listing).

### Fix

`internal/agent/shell/env.go:BuildEnv()` prepends to PATH:

- the directory containing the running outpost binary
  (`os.Executable()` → `filepath.Dir`)
- `$HOME/bin` and `$HOME/.local/bin`
- `/opt/homebrew/{bin,sbin}` and `/usr/local/{bin,sbin}`

Dirs that don't exist on the host are skipped. Entries already on PATH
aren't duplicated. The new env replaces `interp.Env(nil)` in both
`shell/runner.go` (browser `/shell` route) and `agent/ssh.go`
(`/ssh` route's exec channel).

Verified end-to-end on novidesign:

```bash
$ ssh novidesign 'echo "PATH=$PATH" ; which outpost ; ls -la $(which outpost) | head -1'
PATH=/Users/noviadmin/bin:/opt/homebrew/sbin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:/opt/homebrew/bin
/Users/noviadmin/bin/outpost
-rwxr-xr-x  1 noviadmin  staff  39429202 May 23 03:44 /Users/noviadmin/bin/outpost
```

Unit tests in `internal/agent/shell/env_test.go` cover prepend order,
dedup, dir-existence filtering, and PATH preservation.

---

## #8 — `launchctl submit` silently no-ops in the matrix-shell

Unchanged from the prior pass.

### Symptom

```bash
launchctl submit -l my-label -- /path/to/program arg1 arg2
```

returns success but nothing launches — `launchctl list | grep my-label`
shows nothing. The same command run from a normal Terminal session
works fine.

### Root cause (hypothesis)

The SSH session inherits a launchd domain context from the outpost
process. Outpost typically runs as a LaunchDaemon (system domain) on
hosts that need it auto-started. That domain doesn't have `submit`
capability — `launchctl submit` requires either GUI domain
(`gui/$UID`) or explicit bootstrap.

This is not a shell bug. The shell ran what the user typed; launchd
silently refused. There's no "Inappropriate ioctl" or permission
error because launchd considers this a no-op authorization, not a
failure.

### Workaround (works today)

Write a LaunchAgent plist + bootstrap into the user GUI domain:

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

```bash
launchctl bootstrap gui/$UID /tmp/my-label.plist
```

Survives every SSH session lifecycle because launchd owns it under
loginwindow's domain.

### Possible fixes (none implemented)

1. **`outpost run --label X -- <cmd>` helper** — wrap the plist
   generation + bootstrap dance behind a single CLI verb.
2. **Re-bootstrap outpost into the GUI domain at install time** —
   invasive, changes the daemon's plist.
3. **Detect-and-warn** — matrix-shell intercepts `launchctl submit`
   and prints a hint. Hostile to power users.

Option 1 is the most natural; lets agentic flows say
`outpost run --label kg3-pipeline -- ./kg3 serve` and forget about it.
No timeline.
