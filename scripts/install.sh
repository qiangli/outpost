#!/bin/sh
# outpost installer — POSIX one-liner for macOS + Linux.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/qiangli/outpost/main/scripts/install.sh | sh
#   curl -fsSL .../install.sh | INSTALL_DIR=/usr/local/bin sudo -E sh
#   curl -fsSL .../install.sh | OUTPOST_VERSION=v0.3.0 sh
#   curl -fsSL .../install.sh | NO_SERVICE=1 sh
#
# Environment overrides:
#   INSTALL_DIR        target directory (default: $HOME/.local/bin)
#   OUTPOST_VERSION    pin to a tag like v0.3.0 (default: latest release)
#   NO_SERVICE=1       skip launchd / systemd registration
#   REPO               owner/repo (default: qiangli/outpost) — for forks
#
# Why this script (vs. brew/scoop): zero prerequisites beyond a POSIX
# shell + curl-or-wget. Binary is downloaded via curl, which (unlike a
# browser) does not set the macOS quarantine xattr, so Gatekeeper does
# not gate the resulting binary. Linux has no equivalent concern.

set -eu

REPO="${REPO:-qiangli/outpost}"
INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"
OUTPOST_VERSION="${OUTPOST_VERSION:-}"
NO_SERVICE="${NO_SERVICE:-}"

bold=""
dim=""
red=""
green=""
reset=""
if [ -t 1 ]; then
    bold=$(printf '\033[1m')
    dim=$(printf '\033[2m')
    red=$(printf '\033[31m')
    green=$(printf '\033[32m')
    reset=$(printf '\033[0m')
fi

say() { printf '%s\n' "$*"; }
info() { printf '%s==>%s %s\n' "$bold" "$reset" "$*"; }
warn() { printf '%swarning:%s %s\n' "$bold" "$reset" "$*" >&2; }
die() { printf '%serror:%s %s\n' "$red$bold" "$reset" "$*" >&2; exit 1; }
ok() { printf '%s✓%s %s\n' "$green" "$reset" "$*"; }

# ---- 1. detect OS / arch -------------------------------------------------

uname_s=$(uname -s)
uname_m=$(uname -m)

case "$uname_s" in
    Darwin) os=darwin ;;
    Linux)  os=linux ;;
    *) die "unsupported OS: $uname_s (this script handles macOS and Linux; for Windows use install.ps1)" ;;
esac

case "$uname_m" in
    x86_64|amd64) arch=amd64 ;;
    arm64|aarch64) arch=arm64 ;;
    *) die "unsupported architecture: $uname_m (outpost ships amd64 and arm64 only)" ;;
esac

info "platform: $os/$arch"

# ---- 2. resolve target tag -----------------------------------------------

# Pick a downloader. curl is preferred (richer error handling + present
# on macOS by default); wget is the Linux fallback.
if command -v curl >/dev/null 2>&1; then
    fetch() { curl -fsSL "$1" -o "$2"; }
    follow_redirect() { curl -sLI -o /dev/null -w '%{url_effective}\n' "$1"; }
elif command -v wget >/dev/null 2>&1; then
    fetch() { wget -q -O "$2" "$1"; }
    # On wget-only systems we hit the API endpoint instead (rate-limited
    # for anonymous users but acceptable as a fallback).
    follow_redirect() {
        wget -q -O - "https://api.github.com/repos/$REPO/releases/latest" \
            | sed -n 's/.*"tag_name":[[:space:]]*"\([^"]*\)".*/https:\/\/github.com\/'"$REPO"'\/releases\/tag\/\1/p' \
            | head -n1
    }
else
    die "need curl or wget"
fi

if [ -z "$OUTPOST_VERSION" ]; then
    # Follow the /releases/latest redirect to learn the tag without
    # spending a GitHub API request (the API is rate-limited for
    # anonymous users; the redirect is not).
    info "resolving latest release"
    latest_url=$(follow_redirect "https://github.com/$REPO/releases/latest")
    [ -n "$latest_url" ] || die "failed to resolve latest release URL"
    tag=${latest_url##*/}
else
    tag="$OUTPOST_VERSION"
fi
say "  tag: $tag"

# ---- 3. download + verify ------------------------------------------------

asset="outpost-${tag}-${os}-${arch}"
url="https://github.com/${REPO}/releases/download/${tag}/${asset}"
sidecar_url="${url}.sha256"

tmpdir=$(mktemp -d 2>/dev/null || mktemp -d -t outpost-install)
trap 'rm -rf "$tmpdir"' EXIT INT TERM

info "downloading $asset"
fetch "$url" "$tmpdir/$asset" || die "download failed: $url"
fetch "$sidecar_url" "$tmpdir/$asset.sha256" || die "sha256 sidecar download failed: $sidecar_url"

info "verifying sha256"
if command -v sha256sum >/dev/null 2>&1; then
    checker="sha256sum"
elif command -v shasum >/dev/null 2>&1; then
    checker="shasum -a 256"
else
    die "neither sha256sum nor shasum found"
fi
(cd "$tmpdir" && $checker -c "$asset.sha256" >/dev/null) \
    || die "sha256 mismatch — refusing to install (got tampered download?)"
ok "sha256 verified"

# ---- 4. install ----------------------------------------------------------

target="$INSTALL_DIR/outpost"
marker="$INSTALL_DIR/.outpost-installed-via"

# Refuse to overwrite a binary owned by a package manager (brew / scoop /
# apt / …). Same intent as the daemon's installed-via guard — the right
# answer is `brew upgrade outpost`, not us silently clobbering brew's
# record of "what version is installed."
if [ -f "$marker" ]; then
    existing=$(tr -d '[:space:]' <"$marker" 2>/dev/null || true)
    case "$existing" in
        installer|manual|"") ;;
        *) die "outpost at $target was installed via '$existing'; use that package manager to upgrade (or remove $marker to override)" ;;
    esac
fi

if [ ! -d "$INSTALL_DIR" ]; then
    if ! mkdir -p "$INSTALL_DIR" 2>/dev/null; then
        die "cannot create $INSTALL_DIR (run with INSTALL_DIR=/usr/local/bin sudo -E sh ... for a system install)"
    fi
fi

info "installing to $target"
# Move-into-place via rename within the same FS where possible, else
# install(1) to preserve permissions. We chmod first so the live binary
# is executable the moment the rename lands.
chmod +x "$tmpdir/$asset"
if mv -f "$tmpdir/$asset" "$target" 2>/dev/null; then
    :
else
    install -m 0755 "$tmpdir/$asset" "$target" \
        || die "failed to write $target (permissions?)"
fi
printf 'installer\n' >"$marker"
ok "installed $target"

# ---- 5. PATH check -------------------------------------------------------

case ":$PATH:" in
    *":$INSTALL_DIR:"*) ;;
    *)
        warn "$INSTALL_DIR is not on PATH"
        say "  add to your shell rc:"
        say "    ${bold}export PATH=\"$INSTALL_DIR:\$PATH\"${reset}"
        ;;
esac

# ---- 6. service registration ---------------------------------------------

register_service=0
if [ -z "$NO_SERVICE" ] && [ -t 0 ] && [ -t 1 ]; then
    printf 'Register outpost to start at login? [Y/n] '
    read -r ans || ans="n"
    case "$ans" in
        ""|y|Y|yes|YES) register_service=1 ;;
    esac
elif [ -z "$NO_SERVICE" ]; then
    # Non-TTY (curl|sh from CI / piped): register by default. Operators
    # who want to skip pass NO_SERVICE=1 explicitly.
    register_service=1
fi

if [ "$register_service" = "1" ]; then
    # The binary owns the per-platform service definition (launchd / systemd),
    # registering `outpost supervisord` — the always-up parent that keeps the
    # daemon alive. Single source of truth shared with `outpost service install`
    # on already-installed hosts.
    #
    # Privileged (root, e.g. `sudo -E sh`) → system service: starts at BOOT with
    # no login, running as the invoking user (SUDO_USER). Unprivileged → the
    # no-admin per-login fallback, with a hint for how to get boot-persistence.
    if [ "$(id -u)" = "0" ]; then
        info "registering boot service (system — starts at boot, no login)"
        if "$target" service install; then
            ok "service registered (system)"
        else
            warn "service registration failed — register manually with: sudo $target service install"
        fi
    else
        info "registering login service (no admin)"
        if "$target" service install --user; then
            ok "service registered (per-user; starts at login)"
            say "  for start-at-boot without login: ${bold}sudo $target service install${reset}"
        else
            warn "service registration failed — register manually with: $target service install --user"
        fi
    fi
fi

# ---- 7. final hint -------------------------------------------------------

say
ok "outpost is installed."
say
say "Next steps:"
say "  ${bold}outpost register --server https://ai.dhnt.io --code <CODE> --name <hostname>${reset}"
say "    or ${bold}outpost start${reset} and open the admin URL it prints to pair via browser."
say
say "Verify: ${dim}outpost version${reset}"
