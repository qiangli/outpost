# Install & boot-service improvements (backlog)

Concrete, actionable items to make outpost installation — **especially the
Windows boot-service path** — smoother. Distilled from a setup session that
turned what should be a one-command install into a multi-hour ordeal. Ordered
by impact. See [windows-service.md](windows-service.md) for the current manual
recipe and the Windows constraints these items address.

**Shipped so far:** P0b (`schtasks /Run` → `Start-ScheduledTask`),
`scp` posix-rename fallback, keep-awake on install, and
`outpost doctor` / `service status --verbose`. Items below tagged
**✅ shipped** are done; the rest remain open.

## P0 — install is effectively broken on Windows for run-as-a-different-user

**Problem.** `outpost service install` (system mode) assumes an Administrator
can register an S4U `-AtStartup` task for the **run-as** user. On Windows that
returns `Access is denied` — registering S4U for *another* user needs
`SeTcbPrivilege` (SYSTEM only). The command "succeeds" registering a task that
never runs, and there's no signal it's broken (made worse by item P0b).

**Fix.** On Windows, `service install` should:
- detect the run-as-another-user case and perform the registration **in a
  privileged context** (a one-shot `NT AUTHORITY\SYSTEM` proxy task that does the
  `Register-ScheduledTask`), **or** fall back to `-LogonType Password` (which an
  admin *can* register for another user and which auto-grants the batch-logon
  right);
- handle the **regular-user → boot task** case explicitly (grant
  `SeBatchLogonRight`, or fail loudly with guidance, rather than registering a
  task that silently won't run);
- **self-test the registration with `Start-ScheduledTask`** and report whether
  the daemon actually came up.

## P0b — never use `schtasks /Run` ✅ shipped

`schtasks.exe /Run` returns a **false** `0xFFFFFFFF` and spawns nothing while the
task is fine; `Start-ScheduledTask` runs it. Audit our service code, scripts,
and docs to use `Start-ScheduledTask` exclusively for on-demand launch/validate.
A boot fires via the Task Scheduler service (cmdlet path), so a task validated
with `Start-ScheduledTask` will run at boot.

## P0c — `outpost service install --host <remote>` (remote-native install)

The entire premise of the agent is **remote management**, yet the one thing that
*can't* be done remotely today is set up the boot service — operators end up
hand-running PowerShell over the shell. The install logic should run **on the
remote in its native, elevated context** over the existing tunnel/admin surface.
This single feature collapses the bulk of the manual Windows pain.

## P1 — `outpost doctor` / `service status --verbose` ✅ shipped

A one-shot boot-readiness diagnostic that answers: is the boot task registered?
**will it run** (probe via `Start-ScheduledTask`)? is the daemon up, as which
user, with which build? is the host kept awake? Replaces dozens of manual
probes. Cross-platform (also surfaces launchd/systemd state).

## P1 — supervisord-managed routed apps ("managed apps")

Deploying a routed app and making it reboot-surviving is currently two unrelated
steps (register the app route, then hand-build a separate scheduled task /
launchd job for the app process). Let the supervisord own app processes:

```
outpost apps add <name> --command '<exe>' --working-dir <dir> --env ... --managed
```

One step → routed **and** reboot-surviving, with restart/backoff for free. This
is the natural completion of the supervisord work.

## P1 — `outpost scp` posix-rename fallback on Windows ✅ shipped

`scp --safe` stages to `<dst>.new`, sha256-verifies, then atomically renames via
`posix-rename@openssh.com`. The transfer + verify succeed on Windows but the
rename fails with `Access is denied` (Windows SFTP). Fall back to a plain
rename/replace when posix-rename is unsupported. Small fix; today it leaves a
verified `.new` the operator must rename by hand.

## P2 — host-key stability across upgrades

An outpost's SSH host key is meant to be stable across re-pair and self-upgrade
(it's stored outside the pairing config precisely so clients don't see
`REMOTE HOST IDENTIFICATION HAS CHANGED`). A case was observed where it changed
across upgrades. Investigate and add a regression guard.

## P2 — keep-awake during install ✅ shipped

A sleeping host defeats an always-on agent. On Windows, `service install` should
offer to disable idle sleep/hibernate (`powercfg /change standby-timeout-* 0`,
`hibernate-timeout-* 0`) — at minimum warn when aggressive sleep is configured.

## P2 — "clone app from host" deploy flow

Replicating a routed app to another host ("same as on host X") is a common ask.
A helper that copies the app's route config (scheme/host/port, lan-only paths,
index path, identity flags, shared secret) from one paired host to another would
make standing up a QA/staging mirror trivial.

## Docs / skill

- This file + [windows-service.md](windows-service.md) capture the Windows
  reality. Link them from [install.md](install.md) and reference from
  [settings.md](settings.md) where the service settings live.
- A "deploy a cooperative web app to a paired host" skill encoding
  build → deliver → config → `apps add` → browser-verify, with the
  app-config-location gotcha (apps that read config from the working directory,
  not a home dir) called out.
