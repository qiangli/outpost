# Reboot-surviving outpost on Windows

This is the practical runbook for making `outpost` (and outpost-routed apps)
start at boot **with no interactive login**, running as a chosen OS user. macOS
(`launchd` LaunchDaemon + `UserName`) and Linux (`systemd` system unit +
`User=`) are straightforward; **Windows is not**, and the constraints below are
non-obvious enough that they cost real time to discover. Read this before
setting up a Windows host.

## The goal

A Task Scheduler entry that:
- fires **at startup** (`-AtStartup`), so it runs before/without any login;
- runs as the **target user** (so the daemon authenticates incoming sessions
  against *that* user's OS password — outpost's auth model);
- comes back automatically after a reboot.

## The Windows constraints (the hard part)

### 1. Boot-without-login needs admin to register
A task that runs "whether a user is logged on or not" / `-AtStartup` can only be
registered by an **Administrator**. A regular user can register `-AtLogOn`
(interactive) tasks for themselves, but those only fire *after someone logs in*
— they do **not** survive an unattended reboot. There is no no-admin path to
boot-without-login on Windows.

### 2. Logon type: S4U vs Password
The task principal's logon type decides how it runs as the user:

- **S4U** (`-LogonType S4U`) — runs as the user **without storing a password**.
  Preferred. But:
  - Registering an S4U task for a user **other than the caller** requires
    `SeTcbPrivilege`, which **only `NT AUTHORITY\SYSTEM` holds** — *not* even a
    local Administrator. An admin trying to register an S4U task for a different
    user gets `Access is denied`.
  - The target user must hold the **"Log on as a batch job"** right
    (`SeBatchLogonRight`). Regular users lack it by default.
- **Password** (`-User <u> -Password <p> -LogonType Password`) — stores the
  user's password (Windows-encrypted, SYSTEM/admin-readable only). An admin
  **can** register this for another user, and doing so **auto-grants the target
  user the batch-logon right**. The tradeoff is a stored password.

### 3. `schtasks /Run` reports a **false** failure
> **The single biggest time-sink.** `schtasks.exe /Run /TN <name>` can return
> `LastTaskResult = 0xFFFFFFFF` and spawn no process, while the task is
> perfectly fine. The PowerShell cmdlet **`Start-ScheduledTask <name>`** runs
> the *same* task correctly.

Treat a `schtasks /Run` `0xFFFFFFFF` as **inconclusive, not a failure**.
Validate on-demand with `Start-ScheduledTask`. A real boot fires the task via
the Task Scheduler **service** — the same path as the cmdlet, **not**
`schtasks /Run` — so a task that runs via `Start-ScheduledTask` will run at boot.

### 4. The host sleeps
A fresh Windows host sleeps on idle (and on lid-close for laptops), which
silently kills the daemon and the tunnel. Disable it:

```powershell
powercfg /change standby-timeout-ac 0
powercfg /change standby-timeout-dc 0
powercfg /change hibernate-timeout-ac 0
powercfg /change hibernate-timeout-dc 0
```

## The reliable recipe

The cleanest way that avoids the `SeTcbPrivilege` wall: **register the S4U
`-AtStartup` task from the target user's *own elevated* session** — S4U-for-self
needs no special privilege, only an elevated (high-integrity) token and the
batch-logon right (which an admin user has).

1. Make sure the target user is an **administrator** (or otherwise holds "Log on
   as a batch job"). For a regular user, the simplest enabler is local admin.
2. From an **elevated** context **as that user** — e.g. start a temporary,
   elevated `outpost sshd --addr :<port>` as the user and connect to it — run:

   ```powershell
   $exe = "$env:LOCALAPPDATA\outpost\outpost.exe"   # or the real install path
   $a = New-ScheduledTaskAction   -Execute $exe -Argument 'supervisord'
   $t = New-ScheduledTaskTrigger  -AtStartup
   $p = New-ScheduledTaskPrincipal -UserId "$env:USERDOMAIN\$env:USERNAME" `
                                   -LogonType S4U -RunLevel Highest
   $s = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries `
          -DontStopIfGoingOnBatteries -ExecutionTimeLimit ([TimeSpan]::Zero) `
          -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1)
   Register-ScheduledTask -TaskName 'outpost' -Action $a -Trigger $t `
                          -Principal $p -Settings $s -Force
   ```
3. **Validate it actually runs** (this is the proof it'll survive reboot):

   ```powershell
   Start-ScheduledTask outpost      # NOT schtasks /Run
   Start-Sleep 8
   Get-Process outpost -IncludeUserName    # expect supervisord + start as the user
   ```

### Alternative: Password logon
If you can't make the user an admin and don't want to fight the batch-right
grant, an admin can register the task with `-User <u> -Password <p>
-LogonType Password` (no `-Principal`). This stores the password (encrypted) and
auto-grants the batch-logon right. Same `-AtStartup`; same
`Start-ScheduledTask`-not-`schtasks /Run` rule applies.

## Verifying reboot-readiness

```powershell
Get-ScheduledTask outpost | Format-List `
  @{n='runas';e={$_.Principal.UserId}}, `
  @{n='logon';e={$_.Principal.LogonType}}, `
  @{n='trigger';e={$_.Triggers[0].CimClass.CimClassName}}   # MSFT_TaskBootTrigger
```

From a paired peer, confirm the host reports online with the expected build:

```
outpost peers status
```

The same recipe applies to any outpost-routed app that should survive reboot
(register a second `-AtStartup` task whose action is the app binary, with the
app's working directory set) — until supervisord-managed apps land (see
[install-improvements.md](install-improvements.md)).
