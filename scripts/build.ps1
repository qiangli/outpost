# scripts/build.ps1 — build outpost from source on Windows.
#
# PowerShell counterpart of bootstrap-siblings.sh + build.sh in one
# step: materializes the ../sh sibling at the SHA pinned in
# .sibling-pins (go.mod has `replace mvdan.cc/sh/v3 => ../sh`, so a
# bare clone does not build without it), then `go build` with the
# commit + dirty flag stamped so `outpost version` reports a build
# traceable to a git SHA.
#
# Usage (from the repo root):
#   powershell -ExecutionPolicy Bypass -File .\scripts\build.ps1   # → .\bin\outpost.exe
#
# The Bypass wrapper sidesteps Windows' default execution policy
# (which refuses .ps1 files) for this one invocation; with
# `Set-ExecutionPolicy -Scope CurrentUser RemoteSigned` in effect,
# plain `.\scripts\build.ps1` works too. Browser-downloaded ZIPs (not
# git clones) additionally need `Unblock-File` to clear Mark-of-the-Web.
#
# Prerequisites: Go 1.25+ on PATH, plus EITHER system git OR an
# installed outpost (its embedded `outpost git` is used as fallback —
# the zero-system-git self-rebuild path).
#
# Environment overrides (mirror lib.sh):
#   $env:GOOS / $env:GOARCH   # cross-compile target (default: this machine)
#   $env:CGO_ENABLED          # default 0 — outpost has no cgo deps
#   $env:RELEASE_TAG          # additionally stamp a semver tag

[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
# Native exes (git, go) report failures via exit codes we check
# explicitly; don't let PS 7.3+ auto-throw on them.
$PSNativeCommandUseErrorActionPreference = $false

function Info { param([string]$msg) Write-Host "==> $msg" -ForegroundColor Cyan }
function Ok   { param([string]$msg) Write-Host "[ok] $msg"  -ForegroundColor Green }
function Die  { param([string]$msg) Write-Host "error: $msg" -ForegroundColor Red; exit 1 }

$Root = Split-Path -Parent $PSScriptRoot
Set-Location $Root

# ---- 1. pick a git client (system git, else `outpost git`) ---------------

$GitCli = $null
if (Get-Command git -ErrorAction SilentlyContinue) {
    $GitCli = 'system'
} elseif (Get-Command outpost -ErrorAction SilentlyContinue) {
    & outpost git --help *> $null
    if ($LASTEXITCODE -eq 0) { $GitCli = 'outpost' }
}
if (-not $GitCli) { Die "neither system 'git' nor 'outpost git' available — install git or an outpost release first" }

function Git-Clone   { param($Url, $Target)
    if ($GitCli -eq 'system') { & git clone --quiet $Url $Target } else { & outpost git clone --quiet $Url $Target }
    if ($LASTEXITCODE -ne 0) { Die "clone $Url failed" }
}
function Git-Checkout { param($Target, $Sha)
    if ($GitCli -eq 'system') { & git -C $Target checkout --quiet $Sha } else { Push-Location $Target; & outpost git checkout $Sha *> $null; Pop-Location }
    if ($LASTEXITCODE -ne 0) { Die "checkout $Sha in $Target failed" }
}
function Git-ShortHead { param($Target)
    Push-Location $Target
    $sha = if ($GitCli -eq 'system') { & git rev-parse --short HEAD 2>$null } else { & outpost git rev-parse --short HEAD 2>$null }
    Pop-Location
    return "$sha".Trim()
}
function Git-IsDirty { param($Target)
    Push-Location $Target
    $dirty = 'false'
    if ($GitCli -eq 'system') {
        & git diff --quiet 2>$null
        if ($LASTEXITCODE -ne 0) { $dirty = 'true' }
    } else {
        $out = (& outpost git rev-parse --is-dirty 2>$null)
        if ("$out".Trim() -eq 'true') { $dirty = 'true' }
    }
    Pop-Location
    return $dirty
}

# ---- 2. bootstrap siblings from .sibling-pins -----------------------------

$RepoUrls = @{ sh = 'https://github.com/qiangli/sh.git' }

$pins = Join-Path $Root '.sibling-pins'
if (-not (Test-Path $pins)) { Die "missing $pins" }

foreach ($line in Get-Content $pins) {
    $line = $line.Trim()
    if ($line -eq '' -or $line.StartsWith('#')) { continue }
    $name, $sha = $line -split '=', 2
    if (-not $name -or -not $sha) { Die "malformed .sibling-pins line: $line" }

    $target = Join-Path (Split-Path -Parent $Root) $name
    if (Test-Path (Join-Path $target '.git')) {
        Info "sibling $name -> $(Git-ShortHead $target) (already present, leaving alone)"
        continue
    }
    if (-not $RepoUrls.ContainsKey($name)) { Die "no repo URL for sibling '$name'" }
    Info "cloning $($RepoUrls[$name]) -> $target @ $($sha.Substring(0, 12))"
    Git-Clone $RepoUrls[$name] $target
    Git-Checkout $target $sha
}

# ---- 3. build --------------------------------------------------------------

if (-not (Get-Command go -ErrorAction SilentlyContinue)) { Die "Go toolchain not found on PATH — install Go 1.25+ (winget install GoLang.Go)" }

$commit = Git-ShortHead $Root
$dirty  = Git-IsDirty $Root
$ld = "-X github.com/qiangli/outpost/internal/agent.ldCommit=$commit -X github.com/qiangli/outpost/internal/agent.ldDirty=$dirty"
if ($env:RELEASE_TAG) { $ld = "$ld -X github.com/qiangli/outpost/internal/agent.releaseTag=$($env:RELEASE_TAG)" }

if (-not $env:CGO_ENABLED) { $env:CGO_ENABLED = '0' }

$goos = if ($env:GOOS) { $env:GOOS } else { 'windows' }
$out = Join-Path $Root 'bin\outpost'
if ($goos -eq 'windows') { $out = "$out.exe" }
New-Item -ItemType Directory -Force -Path (Join-Path $Root 'bin') | Out-Null

Info "go build ($goos/$(if ($env:GOARCH) { $env:GOARCH } else { 'native' }), commit $commit, dirty=$dirty)"
& go build -ldflags $ld -trimpath -o $out ./cmd/outpost
if ($LASTEXITCODE -ne 0) { Die "go build failed" }
Ok "built $out"
