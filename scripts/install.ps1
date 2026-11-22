# Compatibility bootstrap: one verified bashy archive, then bashy self install.
# Legacy INSTALL_DIR, OUTPOST_VERSION, SERVICE, NO_SERVICE, REPO environment remain valid.
# BASHY_RELEASE_BASE (or -BaseUrl) names a local mirror laid out like the GitHub release
# download base: <base>/latest, <base>/<tag>/<asset>, <base>/<tag>/checksums.txt; an http(s)
# URL or a directory. The mirror supplies the archive AND its checksums;
# BASHY_RELEASE_CHECKSUMS=github keeps the upstream digests instead.
[CmdletBinding()]
param(
    [string]$Dir = '', [string]$Version = '', [string]$BaseUrl = '',
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
if (-not $BaseUrl) { $BaseUrl = $env:BASHY_RELEASE_BASE }
$mirror = $BaseUrl.TrimEnd('/', '\')
$mirrorIsUrl = $mirror -match '^https?://'
function Get-MirrorFile([string]$Path, [string]$Dest) {
    if ($mirrorIsUrl) { Invoke-WebRequest "$mirror/$Path" -OutFile $Dest -UseBasicParsing }
    else { Copy-Item -LiteralPath (Join-Path $mirror $Path) -Destination $Dest }
}
# Default: no service - outpost stays dormant until `bashy login` (the
# `bashy self install` contract). Opt in with -Service or SERVICE=1;
# -NoService and NO_SERVICE=1 force the default.
$installService = [bool]$env:SERVICE
if ($NoService -or $env:NO_SERVICE) { $installService = $false }
if ($Service) { $installService = $true }
if (($User -or $System) -and -not $installService) { throw 'service scope requires service installation' }
$architecture = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
switch ($architecture) { 'AMD64' { $arch = 'amd64' } 'ARM64' { $arch = 'arm64' } default { throw "unsupported architecture: $architecture" } }
if (-not $Version -or $Version -eq 'latest') {
    if ($mirror) { $latestFile = Join-Path ([IO.Path]::GetTempPath()) ('bashy-latest-' + [IO.Path]::GetRandomFileName()); Get-MirrorFile 'latest' $latestFile; $Version = (Get-Content $latestFile -Raw).Trim(); Remove-Item $latestFile -ErrorAction SilentlyContinue }
    else { $release = Invoke-RestMethod "https://api.github.com/repos/$repo/releases/latest"; $Version = $release.tag_name }
}
if ($Version -notmatch '^[A-Za-z0-9._-]+$') { throw 'invalid release tag' }
$asset = "bashy-windows-$arch.zip"
$base = "https://github.com/$repo/releases/download/$Version"
$temp = Join-Path ([IO.Path]::GetTempPath()) ('bashy-install-' + [IO.Path]::GetRandomFileName())
$null = New-Item -ItemType Directory -Path $temp
try {
    $archive = Join-Path $temp $asset
    $checksums = Join-Path $temp 'checksums.txt'
    if ($mirror) {
        Get-MirrorFile "$Version/$asset" $archive
        if ($env:BASHY_RELEASE_CHECKSUMS -eq 'github') { Invoke-WebRequest "$base/checksums.txt" -OutFile $checksums -UseBasicParsing }
        else { Get-MirrorFile "$Version/checksums.txt" $checksums }
    } else {
        Invoke-WebRequest "$base/$asset" -OutFile $archive -UseBasicParsing
        Invoke-WebRequest "$base/checksums.txt" -OutFile $checksums -UseBasicParsing
    }
    $matches = @(Get-Content $checksums | Where-Object { $_ -match ('^[0-9a-fA-F]{64}\s+\*?' + [regex]::Escape($asset) + '$') })
    if ($matches.Count -ne 1) { throw 'missing or duplicate archive checksum' }
    $expected = ($matches[0] -split '\s+')[0]
    if ((Get-FileHash $archive -Algorithm SHA256).Hash -ne $expected) { throw 'archive sha256 mismatch' }
    Expand-Archive $archive -DestinationPath $temp
    $installArgs = @('self', 'install', '--dir', $Dir)
    if ($installService) {
        $installArgs += '--service'
        if ($User) { $installArgs += '--user' } else { $installArgs += '--system' }
    }
    & (Join-Path $temp 'bashy.exe') @installArgs
    if ($LASTEXITCODE -ne 0) { throw "bashy self install failed ($LASTEXITCODE)" }
    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    if (($userPath -split ';') -notcontains $Dir) { [Environment]::SetEnvironmentVariable('Path', (($userPath, $Dir | Where-Object { $_ }) -join ';'), 'User') }
} finally { Remove-Item -Recurse -Force $temp -ErrorAction SilentlyContinue }
