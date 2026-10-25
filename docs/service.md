# Boot service and post-install verification

`outpost service install` installs the supervisor as a boot service on Windows,
macOS, and Linux. It runs as the selected ordinary OS account without requiring
an interactive login. The daemon starts configured internal units, including
`outpost.sshd`; no separate OS registration for that unit is needed. Reinstalling
replaces the managed service definition. Unmanaged running instances must first
be stopped, or explicitly overridden with `--force`.

Install from an elevated PowerShell on Windows. On macOS and Linux, use
`sudo outpost service install`; `SUDO_USER` supplies the service account. When
installing from a root session, supply `--run-as <account>`. Keep the executable
at its permanent location and pair/configure outpost as the service account.

| OS | Default registration | Restart after any exit | Power policy applied by default |
| --- | --- | --- | --- |
| Windows | Task Scheduler `outpost`, at startup, S4U, limited token | Task restart backstop (999 × 1 min) | Standby and hibernate timeouts set to zero for AC and DC; hibernation disabled |
| macOS | `/Library/LaunchDaemons/io.dhnt.outpost.plist`, RunAtLoad and KeepAlive | `KeepAlive` | Sleep and disk sleep zero for every power source; network wake enabled |
| Linux | System `outpost.service`, enabled for multi-user.target | `Restart=always` | Persistent masks for sleep, suspend, hibernate and hybrid-sleep targets |

One lifecycle on all three: the OS service starts `outpost supervisord` at
boot with no login required; the supervisor runs the daemon (`outpost
start`) and the `bashy supervisord` service monitor and restarts either
after any exit. `outpost restart` and `outpost upgrade` go through the
supervisor (the supervised daemon exits and is respawned — it never
re-execs itself). Only the OS-specific install definitions differ; the
supervision behavior is identical.

## Stop vs kill, disable vs stop

Only `outpost stop` writes a stop-intent marker. While the marker is
present the supervisor parks the daemon (no restart); any other exit —
`kill -9`, a crash, even a clean exit — is restarted with backoff. The
intent clears at every supervisor boot, so a reboot always brings the
daemon back.

`outpost disable` writes a persistent marker and stops the daemon: the
supervisor keeps it down across boots until `outpost enable` clears both
markers and starts it again (via the supervisor when one is installed).

`--keep-sleep` preserves power policy. `--dry-run` prints the planned definition
and power commands without applying them. A failed power step returns nonzero
and prints the exact administrator command to run. Installation does not silently
fall back to a session service. Power changes are not undone by uninstall.

The power commands are:

```text
Windows:
  powercfg /change standby-timeout-ac 0
  powercfg /change standby-timeout-dc 0
  powercfg /change hibernate-timeout-ac 0
  powercfg /change hibernate-timeout-dc 0
  powercfg /hibernate off
macOS:
  pmset -a sleep 0 disksleep 0 womp 1
Linux:
  systemctl mask sleep.target suspend.target hibernate.target hybrid-sleep.target
```

These settings address OS idle sleep. Lid closure, firmware behavior, power
loss, and disk-unlock screens are separate from idle policy and require live
verification on the intended hardware.

## Explicit user services

Linux `--user` installs a user unit and enables linger using `loginctl` so the
user manager runs at boot and survives logout. Failure to enable or verify
linger returns nonzero with the exact command to run. The power step still
requires privilege unless `--keep-sleep` is supplied.

On macOS and Windows, `--user` explicitly selects a session service requiring
login. It cannot pass the boot-service check. macOS's default LaunchDaemon cannot
use a locked login Keychain; configure credentials available to unattended
processes. Choose the LaunchAgent fallback only when login Keychain access is
required and session-only operation is acceptable. Auto-login is not required
or configured by the default installation.

Windows S4U installation should run from the target account's own elevated
session; registering for a different account can fail with access denied. See
[Windows service guidance](windows-service.md) for account rights and alternatives.

## Readbacks and reboot evidence

Run these commands as the account that runs the service, using the same cache
environment as that service:

```text
outpost doctor
outpost verify --post-install
outpost verify --post-install --reboot
```

Doctor reports `sleep: disabled|enabled` and `service: boot|session|none`.
Disabled/boot are reported only when the readbacks establish them. Missing tools,
permission errors, malformed output, and incomplete settings fail verification;
inspect the raw JSON for the reason. Windows checks both AC/DC indices and the
hibernate-enabled registry value; macOS checks every `pmset -g custom` power
source; Linux requires persistent masks (runtime masks do not count).

Ordinary verification runs the same six checks on macOS, Linux and
Windows: the service is installed + enabled at boot, the supervisor is
running, the daemon answers healthy, the `outpost.sshd` unit listens on
its loopback port, sleep/hibernate are off, and the boot-session PATH
carries the platform's expected prefixes (healed in-process at
supervisor/daemon boot where the OS service can't pin it — notably
Windows tasks, which carry no environment). It writes
`post-install.json` in the outpost cache directory
(normally `~/.cache/outpost`, or `$XDG_CACHE_HOME/outpost`). It contains
`result: PASS|FAIL`, each check state, and raw command output and exit codes.
A failed check returns nonzero. Opting out of sleep changes does not waive the
sleep verification criterion.

`--reboot` first requires passing settings checks, then writes
`post-install-reboot.json` and `post-install-reboot.pending.json` before scheduling
a reboot in one minute. It does not prompt. Windows uses `shutdown.exe /r /t 60`;
Unix uses `sudo -n shutdown -r +1`, elevating only shutdown so evidence stays in
the service account's cache. If elevation fails, the command returns nonzero and
prints the command to run; the pending request remains available for that retry.

The reboot report remains **FAIL** with phase `awaiting_reboot` or
`awaiting_server` until all checks pass. On the next boot the daemon records:

- A changed OS boot identifier, not merely a process restart.
- Launch by the boot supervisor (the systemd/launchd parent PID, or a Windows
  supervisor in noninteractive session zero).
- No login session observed at daemon startup and at server acknowledgement.
- HTTP 200 from its paired server's authenticated heartbeat endpoint.
- All post-install checks still passing after reconnection (sleep,
  service, supervisor, daemon, sshd, boot PATH — the local liveness
  carries over from the pre-reboot verify).

An observed login or an uncertain session probe remains FAIL, even after logout
and a daemon restart. Session checks are conservative snapshots, not a historical
login audit. The manager must independently confirm unattended startup during
live acceptance. An unpaired or unreachable server cannot produce a reboot PASS.
The completed reboot evidence is retained separately from ordinary verification;
the pending marker is removed after the acknowledged result is saved.

Evidence is local diagnostic data and can contain OS account names, paths, or
other machine details in raw readbacks. Redact it before sharing publicly. No
credentials are added to evidence and no extra server API is needed.

Live acceptance requires the manager to install and verify on approved
non-production Windows, macOS, and Linux hosts, keep interactive sessions closed
through one reboot on each, check server visibility, and confirm configured SSH
availability. Unit and cross-build gates do not substitute for those reboots.

Command references: [Microsoft powercfg](https://learn.microsoft.com/en-us/windows-hardware/design/device-experiences/powercfg-command-line-options),
[systemd enablement](https://www.freedesktop.org/software/systemd/man/latest/systemctl),
[loginctl linger](https://www.freedesktop.org/software/systemd/man/252/loginctl.html),
[Apple pmset source](https://github.com/apple-oss-distributions/PowerManagement/blob/main/pmset/pmset.m).
