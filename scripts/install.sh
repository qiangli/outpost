#!/bin/sh
# Compatibility bootstrap: one verified bashy archive, then the product installer.
# Existing INSTALL_DIR / DHNT_BIN_DIR / OUTPOST_VERSION / NO_SERVICE / REPO work.
set -eu
REPO=${REPO:-qiangli/bashy}
INSTALL_DIR=${INSTALL_DIR:-${DHNT_BIN_DIR:-$HOME/.local/bin}}
release=${BASHY_VERSION:-${OUTPOST_VERSION:-}}
service=1
[ -z "${NO_SERVICE:-}" ] || service=0
scope=
while [ "$#" -gt 0 ]; do
    case "$1" in
        --dir|--version) [ "$#" -ge 2 ] || { echo "$1 needs a value" >&2; exit 2; }
            if [ "$1" = --dir ]; then INSTALL_DIR=$2; else release=$2; fi; shift 2 ;;
        --service) service=1; shift ;;
        --no-service) service=0; shift ;;
        --user|--system) [ -z "$scope" ] || [ "$scope" = "$1" ] || { echo '--user and --system conflict' >&2; exit 2; }; scope=$1; shift ;;
        *) echo "unsupported option: $1 (use --dir, --version, --service, --no-service, --user, --system)" >&2; exit 2 ;;
    esac
done
if [ "$service" = 0 ] && [ -n "$scope" ]; then echo 'service scope requires service installation' >&2; exit 2; fi
case "$(uname -s)" in Darwin) os=darwin ;; Linux) os=linux ;; *) echo 'unsupported OS; Windows uses install.ps1' >&2; exit 1 ;; esac
case "$(uname -m)" in x86_64|amd64) arch=amd64 ;; arm64|aarch64) arch=arm64 ;; *) echo 'unsupported architecture' >&2; exit 1 ;; esac
if command -v curl >/dev/null 2>&1; then
    fetch() { curl -fsSL "$1" -o "$2"; }
elif command -v wget >/dev/null 2>&1; then
    fetch() { wget -q -O "$2" "$1"; }
else echo 'need curl or wget' >&2; exit 1; fi
tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT HUP INT TERM
if [ -z "$release" ] || [ "$release" = latest ]; then
    fetch "https://api.github.com/repos/$REPO/releases/latest" "$tmpdir/release.json"
    release=$(sed -n 's/.*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' "$tmpdir/release.json" | head -n 1)
fi
case "$release" in ''|*[!A-Za-z0-9._-]*) echo 'invalid release tag' >&2; exit 1 ;; esac
asset="bashy-$os-$arch.tar.gz"
base="https://github.com/$REPO/releases/download/$release"
fetch "$base/$asset" "$tmpdir/$asset"
fetch "$base/checksums.txt" "$tmpdir/checksums.txt"
expected=$(awk -v asset="$asset" '$2==asset {print $1}' "$tmpdir/checksums.txt")
[ "${#expected}" = 64 ] || { echo 'missing or duplicate archive checksum' >&2; exit 1; }
if command -v sha256sum >/dev/null 2>&1; then actual=$(sha256sum "$tmpdir/$asset" | awk '{print $1}'); else actual=$(shasum -a 256 "$tmpdir/$asset" | awk '{print $1}'); fi
[ "$actual" = "$expected" ] || { echo 'archive sha256 mismatch' >&2; exit 1; }
tar -xzf "$tmpdir/$asset" -C "$tmpdir" bashy outpost bash sh
set -- self install --dir "$INSTALL_DIR"
if [ "$service" = 1 ]; then
    set -- "$@" --service
    if [ -n "$scope" ]; then set -- "$@" "$scope"; elif [ "$(id -u)" != 0 ]; then set -- "$@" --user; fi
fi
"$tmpdir/bashy" "$@"
case ":$PATH:" in *":$INSTALL_DIR:"*) ;; *) printf 'Add %s to PATH.\n' "$INSTALL_DIR" ;; esac
