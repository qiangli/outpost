#Requires -Version 5.1
<#
.SYNOPSIS
  O4 managed-service sshd proof (Story 1537) — Windows counterpart of
  scripts/sshd-managed-service-check.sh (same steps, same exit codes).

.DESCRIPTION
  Given a bashy+outpost pair (one archive per OS, matched pair), proves
  outpost serves sshd as a managed service installed by bashy:
    1. installs the pair + registers the managed service with
       `bashy self install --service` (per-user logon task by default,
       boot-time task with -Mode system from an elevated prompt);
    2. points the managed daemon at a dedicated ssh_listen_addr port;
    3. logs in over stock ssh with the documented authorized_keys auth,
       runs `echo o4-ok`, and does an SFTP put/get round trip;
    4. stops + uninstalls the service, leaving nothing running.

  Isolation: everything lives under a temp dir; $env:XDG_CONFIG_HOME,
  $env:XDG_CACHE_HOME, $env:BASHY_HOME and $env:OUTPOST_ADMIN_ADDR point
  there. Task Scheduler processes do NOT inherit the caller's process env,
  so the script also sets the same four as USER registry env (reverted on
  exit) BEFORE registering the task — the task process reads them at spawn.
  The daemon's authorized_keys lookup always resolves to the service user's
  real $HOME\.ssh\authorized_keys: one ephemeral key is appended there
  (backed up first, restored on exit).

  The daemon binds ssh_listen_addr on LOOPBACK only (deployment address
  choice, same ServeLANSSH code path as a LAN bind).

  The task name is the fixed `outpost`: if a task of that name already
  exists, the script refuses (exit 2) rather than replacing it.

  ssh/sftp/ssh-keygen ship with Windows 10+ (OpenSSH client capability).

.EXAMPLE
  $env:BASHY_BIN = 'C:\s379-o4\product\bashy.exe'
  .\scripts\sshd-managed-service-check.ps1 -Mode user -Port 22022

.EXIT CODES
  0 PASS; 1 FAIL; 2 usage/config error; 3 SKIP (service manager
  unreachable, e.g. Task Scheduler unavailable in this session).
#>
[CmdletBinding()]
param(
  [string]$BashyBin = $(if ($env:BASHY_BIN) { $env:BASHY_BIN } else { 'bashy' }),
  [string]$ProductDir = '',
  [ValidateSet('user', 'system')]
  [string]$Mode = 'user',
  [int]$Port = $(if ($env:O4_PORT) { [int]$env:O4_PORT } else { 22022 }),
  [int]$AdminPort = $(if ($env:O4_ADMIN_PORT) { [int]$env:O4_ADMIN_PORT } else { 17779 }),
  [int]$TimeoutSecs = $(if ($env:O4_TIMEOUT_SECS) { [int]$env:O4_TIMEOUT_SECS } else { 120 }),
  [int]$SshTimeout = 15,
  [switch]$KeepDir
)

$ErrorActionPreference = 'Stop'
function Fail([string]$m) { Write-Error "FAIL: $m" }
function Info([string]$m) { Write-Host ">> $m" }
function Pass([string]$m) { Write-Host ">> PASS $m" }

# --- 0. preflight ------------------------------------------------------------
foreach ($t in 'ssh', 'sftp', 'ssh-keygen', 'schtasks.exe') {
  if (-not (Get-Command $t -ErrorAction SilentlyContinue)) {
    Fail "missing client tool: $t"; exit 2
  }
}
$bashyCmd = Get-Command $BashyBin -ErrorAction SilentlyContinue
if (-not $bashyCmd) { Fail "BASHY_BIN not found: $BashyBin"; exit 2 }
if (-not $ProductDir) { $ProductDir = Split-Path $bashyCmd.Source }
foreach ($m in 'bashy.exe', 'bash.exe', 'sh.exe', 'outpost.exe') {
  if (-not (Test-Path (Join-Path $ProductDir $m))) {
    Fail "product dir $ProductDir is missing member: $m"; exit 2
  }
}
$BashyBin = Join-Path $ProductDir 'bashy.exe'
Info "product: $ProductDir (bashy+bash+sh+outpost)"

function Test-Tcp([string]$Host_, [int]$P, [int]$Ms = 800) {
  $c = New-Object Net.Sockets.TcpClient
  try {
    $r = $c.BeginConnect($Host_, $P, $null, $null)
    if ($r.AsyncWaitHandle.WaitOne($Ms)) { $c.EndConnect($r); return $true }
    return $false
  } catch { return $false } finally { $c.Close() }
}
if (Test-Tcp '127.0.0.1' $Port) { Fail "port $Port already in use — pick a free -Port"; exit 2 }
if (Test-Tcp '127.0.0.1' $AdminPort) { Fail "admin port $AdminPort already in use — pick a free -AdminPort"; exit 2 }

# Fixed task identity: refuse to replace a registration we did not create.
$taskExists = $false
schtasks.exe /Query /TN outpost 2>$null | Out-Null
if ($LASTEXITCODE -eq 0) { $taskExists = $true }
if ($taskExists) { Fail "Task Scheduler task 'outpost' already registered — remove it first"; exit 2 }

$me = $env:USERNAME
$realHome = [Environment]::GetFolderPath('UserProfile')
$T = Join-Path ([IO.Path]::GetTempPath()) ('o4-sshd-check-' + [IO.Path]::GetRandomFileName())
$null = New-Item -ItemType Directory -Force -Path $T
$Install = Join-Path $T 'install'; $null = New-Item -ItemType Directory -Force -Path $Install
$env:XDG_CONFIG_HOME = Join-Path $T 'config'
$env:XDG_CACHE_HOME = Join-Path $T 'cache'
$env:BASHY_HOME = Join-Path $T 'bhome'
$env:OUTPOST_ADMIN_ADDR = "127.0.0.1:$AdminPort"
# Task processes read USER registry env at spawn — set it BEFORE install.
$propagated = @('XDG_CONFIG_HOME', 'XDG_CACHE_HOME', 'BASHY_HOME', 'OUTPOST_ADMIN_ADDR')
foreach ($n in $propagated) {
  [Environment]::SetEnvironmentVariable($n, [Environment]::GetEnvironmentVariable($n), 'User')
}
$akReal = Join-Path $realHome '.ssh\authorized_keys'
$akExisted = Test-Path $akReal
$akBak = Join-Path $T 'authorized_keys.bak'
if ($akExisted) { Copy-Item $akReal $akBak -Force }

$installed = $false
function Get-InstallProcs {
  # -ErrorAction SilentlyContinue: reading .Path of protected system
  # processes throws; those are never ours, so skip them quietly.
  Get-Process -ErrorAction SilentlyContinue | Where-Object {
    $_.Path -and $_.Path.StartsWith($Install, [StringComparison]::OrdinalIgnoreCase)
  }
}
function Cleanup {
  if ($installed -and (Test-Path (Join-Path $Install 'outpost.exe'))) {
    if ($Mode -eq 'user') { & (Join-Path $Install 'outpost.exe') service uninstall --user 2>$null }
    else { & (Join-Path $Install 'outpost.exe') service uninstall 2>$null }
    & (Join-Path $Install 'outpost.exe') stop 2>$null | Out-Null
  }
  foreach ($n in $propagated) { [Environment]::SetEnvironmentVariable($n, $null, 'User') }
  Get-InstallProcs | ForEach-Object { try { $_ | Stop-Process -Force } catch {} }
  Start-Sleep -Seconds 2
  if ($akExisted) { Copy-Item $akBak $akReal -Force }
  elseif (Test-Path $akReal) { Remove-Item $akReal -Force }
  if ($KeepDir) { Info "keeping $T (-KeepDir)" } else { Remove-Item $T -Recurse -Force -ErrorAction SilentlyContinue }
}

# --- 1. ephemeral key + documented authorized_keys auth ----------------------
try {
  & ssh-keygen -t ed25519 -f (Join-Path $T 'id') -N '' -C o4-check -q
  $sshDir = Split-Path $akReal
  if (-not (Test-Path $sshDir)) { $null = New-Item -ItemType Directory -Force -Path $sshDir }
  Add-Content -Path $akReal -Value (Get-Content (Join-Path $T 'id.pub') -Raw)
  Info 'ephemeral key appended to the service user authorized_keys (backed up, restored on exit)'

  $sshOpts = @('-o', 'BatchMode=yes', '-o', 'IdentitiesOnly=yes',
    '-o', 'PreferredAuthentications=publickey',
    '-o', 'StrictHostKeyChecking=accept-new',
    '-o', "UserKnownHostsFile=$T\known_hosts",
    '-o', "ConnectTimeout=$SshTimeout", '-o', 'LogLevel=ERROR')
  $idFile = Join-Path $T 'id'

  # --- 2. install pair + register managed service via bashy ------------------
  if ($Mode -eq 'user') {
    & $BashyBin self install --dir $Install --service --user
  } else {
    & $BashyBin self install --dir $Install
    & (Join-Path $Install 'outpost.exe') service install --system
  }
  if ($LASTEXITCODE -ne 0) { Fail 'bashy self install (+service) failed'; exit 1 }
  $OutpostBin = Join-Path $Install 'outpost.exe'
  $installed = $true
  Pass "bashy installed the pair + registered the managed service (mode=$Mode)"

  # --- 3. point the managed daemon at the dedicated sshd port ----------------
  function Wait-Tcp([string]$H, [int]$P, [int]$Secs) {
    $end = (Get-Date).AddSeconds($Secs)
    while ((Get-Date) -lt $end) {
      if (Test-Tcp $H $P) { return $true }
      Start-Sleep -Seconds 2
    }
    return $false
  }
  if (-not (Wait-Tcp '127.0.0.1' $AdminPort $TimeoutSecs)) {
    Fail "managed daemon admin never opened 127.0.0.1:$AdminPort within ${TimeoutSecs}s"
    exit 1
  }
  Pass "managed daemon admin answers on 127.0.0.1:$AdminPort"
  & $OutpostBin config set --ssh-listen-addr "127.0.0.1:$Port"
  if ($LASTEXITCODE -ne 0) { Fail 'config set --ssh-listen-addr failed'; exit 1 }
  # Listener binds are read once at boot and nothing auto-restarts for
  # networking changes — restart explicitly (exit-and-respawn under the
  # supervisor; `stop` is the fallback, the supervisor respawns either way).
  & $OutpostBin restart 2>$null | Out-Null
  if ($LASTEXITCODE -ne 0) { & $OutpostBin stop 2>$null | Out-Null }

  if (-not (Wait-Tcp '127.0.0.1' $Port $TimeoutSecs)) {
    Fail "managed sshd never opened 127.0.0.1:$Port within ${TimeoutSecs}s"; exit 1
  }
  Pass "managed sshd listens on 127.0.0.1:$Port"
  if ($Mode -eq 'user') { & $OutpostBin service status --user } else { & $OutpostBin service status }

  # --- 4. ssh in, run a command ----------------------------------------------
  $target = "$me@127.0.0.1"
  $out = & ssh -p $Port -i $idFile @sshOpts $target 'echo o4-ok' 2>&1
  if ($LASTEXITCODE -ne 0) { Fail "ssh exec failed: $out"; exit 1 }
  if ("$out".Trim() -ne 'o4-ok') { Fail "ssh exec printed '$out', want 'o4-ok'"; exit 1 }
  Pass 'ssh exec: echo o4-ok'

  # --- 5. SFTP put/get round trip --------------------------------------------
  'o4-sftp-payload' | Out-File -NoNewline -FilePath (Join-Path $T 'up.txt')
  @("put $T\up.txt $T\rmt.txt", "get $T\rmt.txt $T\down.txt", "rm $T\rmt.txt") |
    Out-File -FilePath (Join-Path $T 'sftp.batch')
  & sftp -P $Port -i $idFile @sshOpts -b (Join-Path $T 'sftp.batch') $target 2>$null | Out-Null
  if ($LASTEXITCODE -ne 0) { Fail 'sftp round trip failed'; exit 1 }
  $up = Get-Content (Join-Path $T 'up.txt') -Raw
  $down = Get-Content (Join-Path $T 'down.txt') -Raw
  if ($up -ne $down) { Fail 'sftp payload mismatch'; exit 1 }
  if (Test-Path (Join-Path $T 'rmt.txt')) { Fail 'sftp cleanup (rm) did not take effect'; exit 1 }
  Pass 'sftp put/get round trip'

  # --- 6. stop + uninstall; nothing may remain --------------------------------
  if ($Mode -eq 'user') { & $OutpostBin service uninstall --user } else { & $OutpostBin service uninstall }
  & $OutpostBin stop 2>$null | Out-Null
  # schtasks /Delete does not stop running processes: stop them by path.
  Get-InstallProcs | ForEach-Object { try { $_ | Stop-Process -Force } catch {} }
  $end = (Get-Date).AddSeconds(60)
  while ((Get-Date) -lt $end -and (Test-Tcp '127.0.0.1' $Port)) { Start-Sleep -Seconds 2 }
  if (Test-Tcp '127.0.0.1' $Port) { Fail 'sshd port still open after uninstall'; exit 1 }
  $left = @(Get-InstallProcs)
  if ($left.Count -gt 0) { Fail "processes still running from ${Install}"; exit 1 }
  $installed = $false
  Pass 'service uninstalled; no listener, no processes, authorized_keys restored'
  Write-Host "O4-OK mode=$Mode port=$Port"
  exit 0
}
catch {
  Fail $_.Exception.Message
  exit 1
}
finally {
  Cleanup
}
