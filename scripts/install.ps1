# outpost installer — PowerShell for Windows.
#
# Usage:
#   iwr -useb https://raw.githubusercontent.com/qiangli/outpost/main/scripts/install.ps1 | iex
#
# Pre-set environment variables before piping into iex to override:
#   $env:INSTALL_DIR     = "C:\Program Files\outpost"   # system-wide
#   $env:OUTPOST_VERSION = "v0.3.0"                     # pin to a tag
#   $env:NO_SERVICE      = "1"                          # skip Task Scheduler
#   $env:REPO            = "qiangli/outpost"            # alternate fork
#
# Why this script (vs. winget/MSI): zero prerequisites and avoids
# Mark-of-the-Web. Invoke-WebRequest does NOT tag downloads with MOTW,
# so SmartScreen will not gate the resulting outpost.exe on first run.
# A browser download of the same file would trigger the warning.
#
# Windows Defender note: outpost opens network sockets and registers a
# startup task — both common malware patterns. Defender may flag it on
# first run. Reputation builds over time via Microsoft submission; see
# docs/install.md for the unblock procedure.

[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
# Disable PS 7.3+'s native-command auto-throw. We want $ErrorActionPreference=Stop
# for PowerShell cmdlets (so a failed Invoke-WebRequest aborts the install) but
# *not* for native exes: schtasks.exe writes "ERROR: The system cannot find the
# file specified" + exit 1 when querying a not-yet-registered task on a fresh
# install, and we explicitly check $LASTEXITCODE where it matters. Without this,
# the installer dies on its first schtasks call. PS 5.1 doesn't have this
# preference variable; the assignment is a harmless no-op there.
$PSNativeCommandUseErrorActionPreference = $false
# Modern TLS for older Windows defaults.
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

function Get-EnvOrDefault {
    param([string]$Name, [string]$Default)
    $v = [Environment]::GetEnvironmentVariable($Name, 'Process')
    if ([string]::IsNullOrEmpty($v)) { return $Default } else { return $v }
}

$Repo           = Get-EnvOrDefault 'REPO' 'qiangli/outpost'
$InstallDir     = Get-EnvOrDefault 'INSTALL_DIR' (Join-Path $env:LOCALAPPDATA 'outpost')
$OutpostVersion = Get-EnvOrDefault 'OUTPOST_VERSION' ''
$NoService      = Get-EnvOrDefault 'NO_SERVICE' ''

function Info { param([string]$msg) Write-Host "==> $msg" -ForegroundColor Cyan }
function Ok   { param([string]$msg) Write-Host "[ok] $msg"  -ForegroundColor Green }
function Warn { param([string]$msg) Write-Warning $msg }
function Die  { param([string]$msg) Write-Host "error: $msg" -ForegroundColor Red; exit 1 }

# ---- 1. detect architecture ---------------------------------------------

switch ($env:PROCESSOR_ARCHITECTURE) {
    'AMD64' { $arch = 'amd64' }
    'ARM64' { $arch = 'arm64' }
    default { Die "unsupported architecture: $env:PROCESSOR_ARCHITECTURE (outpost ships amd64 and arm64 only)" }
}
$os = 'windows'
Info "platform: $os/$arch"

# ---- 2. resolve target tag ----------------------------------------------

if ([string]::IsNullOrEmpty($OutpostVersion)) {
    Info 'resolving latest release'
    # Invoke-WebRequest follows redirects by default; the final URL of
    # /releases/latest is /releases/tag/<tag>. No API call, no rate
    # limit. We use HEAD to avoid downloading the release HTML.
    try {
        $resp = Invoke-WebRequest -Uri "https://github.com/$Repo/releases/latest" `
                                  -Method Head -UseBasicParsing -MaximumRedirection 5
    } catch {
        Die "failed to resolve latest release: $($_.Exception.Message)"
    }
    $finalUrl = $resp.BaseResponse.ResponseUri.AbsoluteUri
    $tag = ($finalUrl -split '/')[-1]
} else {
    $tag = $OutpostVersion
}
Write-Host "  tag: $tag"

# ---- 3. download + verify -----------------------------------------------

$asset       = "outpost-$tag-$os-$arch.exe"
$assetUrl    = "https://github.com/$Repo/releases/download/$tag/$asset"
# Sidecar is named without the .exe suffix — see release.yml's sha256
# step, which writes outpost-<tag>-<os>-<arch>.sha256.
$sidecarUrl  = "https://github.com/$Repo/releases/download/$tag/outpost-$tag-$os-$arch.sha256"
$tmpDir      = Join-Path ([System.IO.Path]::GetTempPath()) ("outpost-install-" + [System.IO.Path]::GetRandomFileName())
$null        = New-Item -ItemType Directory -Path $tmpDir
try {
    $assetPath   = Join-Path $tmpDir $asset
    $sidecarPath = Join-Path $tmpDir "$asset.sha256"

    Info "downloading $asset"
    try {
        Invoke-WebRequest -Uri $assetUrl   -OutFile $assetPath   -UseBasicParsing
        Invoke-WebRequest -Uri $sidecarUrl -OutFile $sidecarPath -UseBasicParsing
    } catch {
        Die "download failed: $($_.Exception.Message)"
    }

    Info 'verifying sha256'
    # The sidecar format is "<hex>  <filename>" per `shasum -a 256` in
    # the release workflow. Read the hex and compare against the actual
    # file hash. Case-insensitive (hex is hex).
    $expected = (Get-Content $sidecarPath -First 1).Split(' ')[0].ToLower()
    $actual   = (Get-FileHash -Path $assetPath -Algorithm SHA256).Hash.ToLower()
    if ($expected -ne $actual) {
        Die "sha256 mismatch — refusing to install (got tampered download?): expected=$expected got=$actual"
    }
    Ok 'sha256 verified'

    # ---- 4. install -----------------------------------------------------

    $target = Join-Path $InstallDir 'outpost.exe'
    $marker = Join-Path $InstallDir '.outpost-installed-via'

    # Refuse to overwrite a package-manager-owned install. Mirror of the
    # daemon-side guard in internal/agent/upgrade/worker.go.
    if (Test-Path $marker) {
        $existing = (Get-Content $marker -First 1 -ErrorAction SilentlyContinue).Trim().ToLower()
        if ($existing -and $existing -ne 'installer' -and $existing -ne 'manual') {
            Die "outpost at $target was installed via '$existing'; use that package manager to upgrade (or remove $marker to override)"
        }
    }

    if (-not (Test-Path $InstallDir)) {
        try {
            $null = New-Item -ItemType Directory -Path $InstallDir -Force
        } catch {
            Die "cannot create ${InstallDir}: $($_.Exception.Message)"
        }
    }

    Info "installing to $target"
    # Stop a running outpost so the binary file isn't locked while we
    # replace it. Stop-Process is sufficient: /Create below uses /F to
    # overwrite any existing task entry, so calling schtasks /End first
    # adds nothing and would fail awkwardly on a fresh install (no task
    # registered yet).
    Get-Process -Name outpost -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
    Start-Sleep -Milliseconds 200
    try {
        Move-Item -Path $assetPath -Destination $target -Force
    } catch {
        Die "failed to write ${target}: $($_.Exception.Message)"
    }
    Set-Content -Path $marker -Value 'installer' -NoNewline -Encoding ascii
    Ok "installed $target"

    # ---- 5. PATH check --------------------------------------------------

    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    $pathParts = if ($userPath) { $userPath -split ';' } else { @() }
    if ($pathParts -notcontains $InstallDir) {
        Info "adding $InstallDir to user PATH"
        $newPath = if ($userPath) { "$userPath;$InstallDir" } else { $InstallDir }
        [Environment]::SetEnvironmentVariable('Path', $newPath, 'User')
        # The current shell won't see the change until restart; tell the
        # user once instead of silently leaving them confused.
        Warn "PATH updated — open a new PowerShell window for it to take effect."
    }

    # ---- 6. service registration ---------------------------------------

    $registerService = $false
    if ([string]::IsNullOrEmpty($NoService)) {
        if ([Environment]::UserInteractive -and -not [Console]::IsInputRedirected) {
            $ans = Read-Host 'Register outpost to start at logon? [Y/n]'
            if ([string]::IsNullOrEmpty($ans) -or $ans -match '^[yY]') {
                $registerService = $true
            }
        } else {
            # Non-interactive (iwr | iex from a pipeline / CI): register
            # by default. Operators who want to skip pass NO_SERVICE=1.
            $registerService = $true
        }
    }

    if ($registerService) {
        Info 'registering Task Scheduler entry (ONLOGON)'
        # /SC ONLOGON + /RL LIMITED registers the task in the current
        # user's context, no admin elevation required, starts on user
        # logon. This is the analogue of "launchctl bootstrap gui/<uid>"
        # / "systemctl --user enable --now" on the other platforms.
        # /F overwrites any prior registration so a re-install picks up
        # the new path. The /TR argument wraps $target in quotes so a
        # path containing spaces (e.g. "C:\Program Files\outpost") is
        # parsed as a single program token by Task Scheduler.
        $tr = '"' + $target + '" start'
        $output = & schtasks.exe /Create /SC ONLOGON /RL LIMITED /TN outpost /TR $tr /F 2>&1
        if ($LASTEXITCODE -ne 0) {
            Warn "schtasks failed: $output"
            Write-Host '  Register manually with:'
            Write-Host "    schtasks /Create /SC ONLOGON /RL LIMITED /TN outpost /TR `"$tr`" /F"
        } else {
            Ok 'outpost task registered (run on logon)'
            Write-Host '  status: schtasks /Query /TN outpost'
            Write-Host '  remove: schtasks /Delete /TN outpost /F'
        }
    }
} finally {
    Remove-Item -Recurse -Force $tmpDir -ErrorAction SilentlyContinue
}

# ---- 7. final hint ------------------------------------------------------

Write-Host ''
Ok 'outpost is installed.'
Write-Host ''
Write-Host 'Next steps:'
Write-Host '  outpost register --server https://ai.dhnt.io --code <CODE> --name <hostname>'
Write-Host '    or `outpost start` and open the admin URL it prints to pair via browser.'
Write-Host ''
Write-Host 'Verify: outpost version'
Write-Host ''
Write-Host 'If Windows Defender or SmartScreen flags outpost on first run, see' -ForegroundColor DarkGray
Write-Host "  https://github.com/$Repo/blob/main/docs/install.md#windows-defender" -ForegroundColor DarkGray
