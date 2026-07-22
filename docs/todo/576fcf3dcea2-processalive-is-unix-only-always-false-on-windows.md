---
id: 576fcf3dcea2
kind: task
title: 'processAlive is Unix-only: always false on Windows, breaking the duplicate-instance guard'
seq: 5
status: todo
priority: p2
created: 2026-07-22T06:23:00.101331Z
---

cmd/outpost/main.go:3317 probes liveness with p.Signal(syscall.Signal(0)). That is the POSIX idiom; on Windows Go's Process.Signal rejects everything except Kill, so the call always errors and processAlive() always returns FALSE. There is no _windows.go variant (only detach_windows.go and service_windows.go exist).

Observed on puppy (windows/amd64, outpost v0.14.4) 2026-07-22:

  tasklist            -> outpost.exe PID 3256 (supervisord), PID 6268 (daemon)
  supervisord.pid     -> 3256
  outpost supervisord status -> 'not running (stale pidfile, last pid 3256)'

A live process reported dead.

The status misreport is cosmetic, but the same function backs guards that are not:

  - claimPidFile (main.go:3348) — the refuse-a-second-instance check. On Windows
    it can never fire, so two daemons can race the same pidfile and the matrix
    tunnel's fixed remote port. On Unix this correctly prints
    'outpost is already running (pid N). Stop it first'.
  - the post-spawn liveness check (main.go:3287) and outpost stop's SIGTERM poll
    (3387/3409) also read it.

Fix: add a build-tagged Windows implementation. OpenProcess + GetExitCodeProcess
(STILL_ACTIVE) is the usual approach; note os.FindProcess on Windows already
opens a handle and errors for a missing pid, so even that alone is closer to
correct than Signal(0).

Add a test that does not silently pass on Unix — the current behavior is
invisible to a macOS/Linux CI run, which is why it survived.
