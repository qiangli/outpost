# Installing outpost

Install bashy to get bashy and outpost together. The canonical install commands,
Homebrew and Scoop channels, Linux package and tarball options, and current
unsigned-binary guidance are in the [outpost README](../README.md#install).

The macOS/Linux script installer downloads the matching bashy release archive,
verifies it against `checksums.txt`, and runs `bashy self install`. The Windows
PowerShell installer follows the same checksum-verified flow for the Windows
zip. Both installers can register outpost as a per-user service/task.

Environment overrides supported by the compatibility scripts:

| Variable | Effect |
|----------|--------|
| `INSTALL_DIR` | Install directory. |
| `BASHY_VERSION` or `OUTPOST_VERSION` | Pin a release tag. |
| `NO_SERVICE` | When set, skip service/task installation. |
| `REPO` | Use an alternate bashy repository. |

To install without registering a service, use `NO_SERVICE=1` with the Unix
script or `$env:NO_SERVICE = '1'` before the PowerShell command. Linux headless
hosts may need `loginctl enable-linger` for the user service to run after logout.

v1.0 macOS and Windows binaries are not yet code-signed. Script and package
manager installs do not set browser quarantine/Mark-of-the-Web; a browser
downloaded archive can prompt at first launch. Windows Smart App Control or
Defender may still evaluate or block an unsigned executable according to the
host's security policy. DHNT Inc.-signed builds arrive in v1.0.x.

### Windows Defender

Smart App Control and Defender can evaluate unsigned executables under the
machine's security policy. v1.0 macOS and Windows binaries are unsigned;
DHNT Inc.-signed builds arrive in v1.0.x. If Defender identifies a release
binary, report it at <https://submit.microsoft.com> and follow the machine
administrator's policy rather than disabling protection globally.

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

**Self-upgrade behaviour**: `go install` builds don't write the `.outpost-installed-via` marker file, and upgrades always apply (there is no `update_mode` opt-out — see `outpost docs settings`). A dirty working tree still refuses cloudbox-pushed upgrades, so commit before releasing from a dev host.

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
