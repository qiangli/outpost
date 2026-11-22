#!/bin/sh
# Compatibility bootstrap: one verified bashy archive, then the product installer.
# Existing INSTALL_DIR / DHNT_BIN_DIR / OUTPOST_VERSION / SERVICE / NO_SERVICE / REPO work.
# BASHY_RELEASE_BASE (or --base-url) names a local mirror laid out like the GitHub
# release download base: <base>/latest (the tag), <base>/<tag>/<asset>,
# <base>/<tag>/checksums.txt. It may be an http(s) URL or a local directory. The
# mirror then supplies the archive AND its checksums (it is the trust anchor on
# that LAN); BASHY_RELEASE_CHECKSUMS=github keeps the upstream digests instead.
set -eu
REPO=${REPO:-qiangli/bashy}
mirror=${BASHY_RELEASE_BASE:-}
INSTALL_DIR=${INSTALL_DIR:-${DHNT_BIN_DIR:-$HOME/.local/bin}}
release=${BASHY_VERSION:-${OUTPOST_VERSION:-}}
# Default: no service - outpost stays dormant until `bashy login` (the
# `bashy self install` contract). Opt in with --service or SERVICE=1;
# --no-service and NO_SERVICE=1 force the default.
service=0
[ -z "${SERVICE:-}" ] || service=1
[ -z "${NO_SERVICE:-}" ] || service=0
scope=
while [ "$#" -gt 0 ]; do
    case "$1" in
        --dir|--version|--base-url) [ "$#" -ge 2 ] || { echo "$1 needs a value" >&2; exit 2; }
            case "$1" in --dir) INSTALL_DIR=$2 ;; --version) release=$2 ;; *) mirror=$2 ;; esac; shift 2 ;;
        --service) service=1; shift ;;
        --no-service) service=0; shift ;;
        --user|--system) [ -z "$scope" ] || [ "$scope" = "$1" ] || { echo '--user and --system conflict' >&2; exit 2; }; scope=$1; shift ;;
        *) echo "unsupported option: $1 (use --dir, --version, --base-url, --service, --no-service, --user, --system)" >&2; exit 2 ;;
    esac
done
if [ "$service" = 0 ] && [ -n "$scope" ]; then echo 'service scope requires service installation' >&2; exit 2; fi
if [ "$service" = 1 ] && [ "$(id -u)" = 0 ]; then
    echo 'Run the installer as the regular account that will run Outpost.' >&2
    exit 1
fi
case "$(uname -s)" in Darwin) os=darwin ;; Linux) os=linux ;; *) echo 'unsupported OS; Windows uses install.ps1' >&2; exit 1 ;; esac
case "$(uname -m)" in x86_64|amd64) arch=amd64 ;; arm64|aarch64) arch=arm64 ;; *) echo 'unsupported architecture' >&2; exit 1 ;; esac
if command -v curl >/dev/null 2>&1; then
    fetch() { curl -fsSL "$1" -o "$2"; }
elif command -v wget >/dev/null 2>&1; then
    fetch() { wget -q -O "$2" "$1"; }
else echo 'need curl or wget' >&2; exit 1; fi
tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT HUP INT TERM
# mirror_fetch PATH DEST: read <mirror>/PATH over http(s) or from a directory.
case "$mirror" in
    '') mirror_fetch() { echo 'no mirror configured' >&2; exit 1; } ;;
    http://*|https://*) mirror_fetch() { fetch "${mirror%/}/$1" "$2"; } ;;
    *) [ -d "$mirror" ] || { echo "mirror directory not found: $mirror" >&2; exit 1; }
       mirror_fetch() { cp -- "${mirror%/}/$1" "$2"; } ;;
esac
if [ -z "$release" ] || [ "$release" = latest ]; then
    if [ -n "$mirror" ]; then
        mirror_fetch latest "$tmpdir/latest"
        release=$(tr -d '[:space:]' < "$tmpdir/latest")
    else
        fetch "https://api.github.com/repos/$REPO/releases/latest" "$tmpdir/release.json"
        release=$(sed -n 's/.*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' "$tmpdir/release.json" | head -n 1)
    fi
fi
case "$release" in ''|*[!A-Za-z0-9._-]*) echo 'invalid release tag' >&2; exit 1 ;; esac
asset="bashy-$os-$arch.tar.gz"
base="https://github.com/$REPO/releases/download/$release"
if [ -n "$mirror" ]; then
    mirror_fetch "$release/$asset" "$tmpdir/$asset"
    if [ "${BASHY_RELEASE_CHECKSUMS:-mirror}" = github ]; then
        fetch "$base/checksums.txt" "$tmpdir/checksums.txt"
    else
        mirror_fetch "$release/checksums.txt" "$tmpdir/checksums.txt"
    fi
else
    fetch "$base/$asset" "$tmpdir/$asset"
    fetch "$base/checksums.txt" "$tmpdir/checksums.txt"
fi
expected=$(awk -v asset="$asset" '$2==asset {print $1}' "$tmpdir/checksums.txt")
[ "${#expected}" = 64 ] || { echo 'missing or duplicate archive checksum' >&2; exit 1; }
if command -v sha256sum >/dev/null 2>&1; then actual=$(sha256sum "$tmpdir/$asset" | awk '{print $1}'); else actual=$(shasum -a 256 "$tmpdir/$asset" | awk '{print $1}'); fi
[ "$actual" = "$expected" ] || { echo 'archive sha256 mismatch' >&2; exit 1; }
tar -xzf "$tmpdir/$asset" -C "$tmpdir" bashy outpost bash sh
set -- self install --dir "$INSTALL_DIR"
if [ "$scope" = --user ]; then
    set -- "$@" --service --user
fi
"$tmpdir/bashy" "$@"
if [ "$service" = 1 ] && [ "$scope" != --user ]; then
    # Register the installed executable via sudo, including with older releases
    # whose self install command does not elevate system service registration.
    installed_dir=$(CDPATH= cd -- "$INSTALL_DIR" && pwd -P)
    sudo -- "$installed_dir/outpost" service install --system
fi
case ":$PATH:" in *":$INSTALL_DIR:"*) ;; *) printf 'Add %s to PATH.\n' "$INSTALL_DIR" ;; esac
