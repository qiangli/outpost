# Installing outpost

Outpost ships as a single self-contained binary (`outpost`). There are three supported install paths — pick the one that matches your environment.

| Path | Audience | Auto-update | Linux OS-password auth |
|------|----------|-------------|------------------------|
| `install.sh` (curl-pipe) | macOS / Linux, anyone | cloudbox push | yes (pure-Go unix_chkpwd/shadow, no CGO) |
| `install.ps1` (PowerShell) | Windows | cloudbox push | n/a |
| `go install` | developers on any OS | manual rebuild | yes (no CGO needed) |

The pre-built binary releases live on [GitHub Releases](https://github.com/qiangli/outpost/releases). Every release ships six artifacts (`darwin/linux/windows × arm64/amd64`) plus a matching `.sha256` sidecar per artifact.

## macOS and Linux: `install.sh`

```sh
curl -fsSL https://raw.githubusercontent.com/qiangli/outpost/main/scripts/install.sh | sh
```

What it does:

1. Detects your OS and architecture.
2. Resolves the latest release tag (no GitHub API call — follows the `/releases/latest` redirect).
3. Downloads the matching binary and its `.sha256` sidecar.
4. Verifies the sha256.
5. Installs to `$HOME/.local/bin/outpost` and writes a small marker file (`.outpost-installed-via`) so future package-manager installs and the cloudbox-pushed self-upgrade behave consistently.
6. Offers to register the daemon to start at login — macOS via `launchctl bootstrap gui/<uid>` (a per-user LaunchAgent at `~/Library/LaunchAgents/io.dhnt.outpost.plist`), Linux via `systemctl --user enable --now outpost.service` (unit at `~/.config/systemd/user/outpost.service`).

Environment overrides:

| Variable | Default | Effect |
|----------|---------|--------|
| `INSTALL_DIR` | `$HOME/.local/bin` | Target directory. Set to `/usr/local/bin` (with `sudo -E`) for a system-wide install. |
| `OUTPOST_VERSION` | latest | Pin a specific tag, e.g. `v0.3.0`. |
| `NO_SERVICE` | unset | When `1`, skip the launchd / systemd registration step. |
| `REPO` | `qiangli/outpost` | Alternate fork. |

Why curl-pipe is safe here: `curl` does **not** set the macOS `com.apple.quarantine` extended attribute, so Gatekeeper will not gate the downloaded binary on first run. A browser download of the same file *would* trigger the "cannot verify developer" dialog. On Linux there is no equivalent concern.

**Headless Linux note**: if there is no graphical login on the box, run `sudo loginctl enable-linger $(id -un)` so the user unit keeps running after logout. The script prints this hint at the end of the install when relevant.

## Windows: `install.ps1`

```powershell
iwr -useb https://raw.githubusercontent.com/qiangli/outpost/main/scripts/install.ps1 | iex
```

What it does:

1. Detects architecture.
2. Resolves the latest release tag via `Invoke-WebRequest -Method Head` (which follows redirects).
3. Downloads the `.exe` and the `.sha256` sidecar.
4. Verifies via `Get-FileHash`.
5. Installs to `%LOCALAPPDATA%\outpost\outpost.exe` and adds the directory to the user `PATH`.
6. Offers to register a Task Scheduler entry that runs `outpost start` at logon (PowerShell `Register-ScheduledTask -TaskName outpost -AtLogOn -RunLevel Limited` — the COM-API path, used because `schtasks /Create /TR "..."` breaks on user paths with spaces). Status / remove via `schtasks /Query /TN outpost` and `schtasks /Delete /TN outpost /F`.

Overrides (set the variable in your shell before piping into `iex`):

| Variable | Default | Effect |
|----------|---------|--------|
| `$env:INSTALL_DIR` | `%LOCALAPPDATA%\outpost` | Target directory. Set to `C:\Program Files\outpost` for a system install (admin shell). |
| `$env:OUTPOST_VERSION` | latest | Pin a specific tag. |
| `$env:NO_SERVICE` | unset | When `1`, skip Task Scheduler registration. |
| `$env:REPO` | `qiangli/outpost` | Alternate fork. |

`Invoke-WebRequest` does not tag downloads with the Mark-of-the-Web zone identifier, so SmartScreen will not show the "Windows protected your PC" dialog when the resulting `outpost.exe` runs.

### Windows Defender

Outpost opens network sockets and registers a startup task. Both behaviours are common in malware and Defender's heuristic scan may flag the binary on first run — anywhere from a benign notification to outright quarantine, depending on policy.

Mitigation options, in increasing cost:

1. **Whitelist locally.** In *Windows Security → Virus & threat protection → Exclusions*, add `%LOCALAPPDATA%\outpost\outpost.exe`.
2. **Report a false positive.** Submit the binary at <https://submit.microsoft.com>. Repeat for a few releases — Microsoft's reputation system tracks publisher behaviour, and persistent good-faith submissions clear the flag over time.
3. **Code-signing certificate.** A standard Authenticode cert (~$75–200/yr) cuts SmartScreen friction for new binaries once you've built reputation; an EV cert (~$300–700/yr) clears SmartScreen immediately. This is on the roadmap once user demand justifies it.

## Developers: `go install`

If you already have a Go 1.25+ toolchain:

```sh
go install github.com/qiangli/outpost/cmd/outpost@latest
# or pin to a tag
go install github.com/qiangli/outpost/cmd/outpost@v0.3.0
```

The resulting binary lands in `$GOBIN` (or `$GOPATH/bin`, or `$HOME/go/bin`) — make sure that directory is on `PATH`.

**Linux OS-password authentication**: works out of the box in the official
`CGO_ENABLED=0` releases — no source build, no `libpam-dev`, no CGO. The Linux
authenticator is pure Go: it shells out to the setuid `unix_chkpwd` PAM helper
and, if that rejects a valid password (Ubuntu 26.04 / PAM 1.7.0 ship a broken
one), falls back to reading `/etc/shadow` and verifying the crypt(3) hash
directly. The shadow fallback needs the outpost process to be able to read
`/etc/shadow` (shadow-group membership, or running as root); when it can't,
only the `unix_chkpwd` path is available. Accounts backed by LDAP/SSSD/AD
rather than local `/etc/shadow` should use `AUTH_URL`-mode authentication.

`go install` builds do not include the `releaseTag` ldflag stamp that GitHub Releases bake in, so `outpost version` will report the commit short-sha rather than `vX.Y.Z`. Use `outpost version --json` for the full provenance.

**Self-upgrade behaviour**: `go install` builds don't write the `.outpost-installed-via` marker file. Whether the cloudbox-pushed upgrade flow runs is controlled by `update_mode` in `agent.json` (see `outpost docs settings`). For development hosts you typically want `update_mode=never`.

## Verifying an install

After any of the above:

```sh
outpost version --json   # full BuildInfo struct
outpost status           # paired-yet? builtins on? apps registered?
outpost docs install     # this page, embedded in the binary
```

## Pairing

The install puts outpost on disk and (optionally) starts it. It does not pair you with a portal — that's an explicit step:

```sh
# CLI one-shot (paste the code your portal shows):
outpost register --server https://ai.dhnt.io --code <CODE> --name <hostname>

# or, if outpost is already running unpaired, open the admin URL it
# printed to stdout — the SPA has a "Pair" tab that walks through it.
```

The admin UI is loopback-only by default (`127.0.0.1:17777`). Pairing triggers a self-restart so the matrix tunnel can come up against the cloud portal.

## Uninstalling

There is no `outpost uninstall` subcommand yet (tracked for a later release). Manual steps:

**macOS**:
```sh
launchctl bootout gui/$(id -u)/io.dhnt.outpost
rm ~/Library/LaunchAgents/io.dhnt.outpost.plist
rm "$HOME/.local/bin/outpost" "$HOME/.local/bin/.outpost-installed-via"
# Canonical XDG-style paths (used by all current installs):
rm -rf "$HOME/.config/matrix"                       # config (paired identity, tokens)
rm -rf "$HOME/.cache/outpost"                       # pidfile, upgrade ledger, shell history
# Legacy paths (only present on pre-migration installs; harmless if absent):
rm -rf "$HOME/Library/Application Support/matrix" "$HOME/Library/Caches/outpost"
```

**Linux**:
```sh
systemctl --user disable --now outpost.service
rm ~/.config/systemd/user/outpost.service
systemctl --user daemon-reload
rm "$HOME/.local/bin/outpost" "$HOME/.local/bin/.outpost-installed-via"
rm -rf "${XDG_CONFIG_HOME:-$HOME/.config}/matrix"
rm -rf "${XDG_CACHE_HOME:-$HOME/.cache}/outpost"
```

**Windows** (PowerShell):
```powershell
schtasks /Delete /TN outpost /F
Stop-Process -Name outpost -ErrorAction SilentlyContinue
Remove-Item -Recurse -Force "$env:LOCALAPPDATA\outpost"
# Canonical XDG-style path (used by all current installs):
Remove-Item -Recurse -Force "$env:USERPROFILE\.config\matrix"
# Legacy path (only present on pre-migration installs; harmless if absent):
Remove-Item -Recurse -Force "$env:APPDATA\matrix" -ErrorAction SilentlyContinue
```
