# Compatibility bootstrap: one verified bashy archive, then bashy self install.
# Legacy INSTALL_DIR, OUTPOST_VERSION, NO_SERVICE, REPO environment remain valid.
[CmdletBinding()]
param(
    [string]$Dir = '', [string]$Version = '',
    [switch]$Service, [switch]$NoService, [switch]$User, [switch]$System
)
$ErrorActionPreference = 'Stop'
$PSNativeCommandUseErrorActionPreference = $false
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
if ($User -and $System) { throw '-User and -System are mutually exclusive' }
if ($Service -and $NoService) { throw '-Service and -NoService are mutually exclusive' }
$repo = if ($env:REPO) { $env:REPO } else { 'qiangli/bashy' }
if (-not $Dir) { $Dir = if ($env:INSTALL_DIR) { $env:INSTALL_DIR } elseif ($env:DHNT_BIN_DIR) { $env:DHNT_BIN_DIR } else { Join-Path $env:LOCALAPPDATA 'outpost' } }
if (-not $Version) { $Version = if ($env:BASHY_VERSION) { $env:BASHY_VERSION } else { $env:OUTPOST_VERSION } }
$installService = -not ($NoService -or $env:NO_SERVICE)
if ($Service) { $installService = $true }
if (($User -or $System) -and -not $installService) { throw 'service scope requires service installation' }
$architecture = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
switch ($architecture) { 'AMD64' { $arch = 'amd64' } 'ARM64' { $arch = 'arm64' } default { throw "unsupported architecture: $architecture" } }
if (-not $Version -or $Version -eq 'latest') { $release = Invoke-RestMethod "https://api.github.com/repos/$repo/releases/latest"; $Version = $release.tag_name }
if ($Version -notmatch '^[A-Za-z0-9._-]+$') { throw 'invalid release tag' }
$asset = "bashy-windows-$arch.zip"
$base = "https://github.com/$repo/releases/download/$Version"
$temp = Join-Path ([IO.Path]::GetTempPath()) ('bashy-install-' + [IO.Path]::GetRandomFileName())
$null = New-Item -ItemType Directory -Path $temp
try {
    $archive = Join-Path $temp $asset
    Invoke-WebRequest "$base/$asset" -OutFile $archive -UseBasicParsing
    $checksums = Join-Path $temp 'checksums.txt'
    Invoke-WebRequest "$base/checksums.txt" -OutFile $checksums -UseBasicParsing
    $matches = @(Get-Content $checksums | Where-Object { $_ -match ('^[0-9a-fA-F]{64}\s+\*?' + [regex]::Escape($asset) + '$') })
    if ($matches.Count -ne 1) { throw 'missing or duplicate archive checksum' }
    $expected = ($matches[0] -split '\s+')[0]
    if ((Get-FileHash $archive -Algorithm SHA256).Hash -ne $expected) { throw 'archive sha256 mismatch' }
    Expand-Archive $archive -DestinationPath $temp
    $installArgs = @('self', 'install', '--dir', $Dir)
    if ($installService) {
        $installArgs += '--service'
        $isAdmin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole([Security.Principal.WindowsBuiltinRole]::Administrator)
        if ($User -or (-not $System -and -not $isAdmin)) { $installArgs += '--user' } elseif ($System) { $installArgs += '--system' }
    }
    & (Join-Path $temp 'bashy.exe') @installArgs
    if ($LASTEXITCODE -ne 0) { throw "bashy self install failed ($LASTEXITCODE)" }
    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    if (($userPath -split ';') -notcontains $Dir) { [Environment]::SetEnvironmentVariable('Path', (($userPath, $Dir | Where-Object { $_ }) -join ';'), 'User') }
} finally { Remove-Item -Recurse -Force $temp -ErrorAction SilentlyContinue }
