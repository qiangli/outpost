#!/usr/bin/env bash
# scripts/install-bin.sh — build outpost + install it into $INSTALL_DIR.
# Replaces `make install`.
#
# Name is `install-bin.sh` (not `install.sh`) to avoid colliding with the
# existing scripts/install.sh, which is the end-user curl-installer.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
cd "$ROOT"
# shellcheck source=lib.sh
. "$ROOT/scripts/lib.sh"

"$ROOT/scripts/build.sh"

mkdir -p "$INSTALL_DIR"
install_tmp="$INSTALL_DIR/.$BIN.new.$$"
trap 'rm -f "$install_tmp"' EXIT
cp "$OUT_DIR/$BIN" "$install_tmp"
chmod 0755 "$install_tmp"
# Replace the directory entry atomically. Overwriting an executable in place
# can invalidate the code-signing pages of a currently running macOS daemon
# and make the newly installed binary die with SIGKILL.
mv -f "$install_tmp" "$INSTALL_DIR/$BIN"
trap - EXIT
echo "installed $INSTALL_DIR/$BIN"
