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
cp -f "$OUT_DIR/$BIN" "$INSTALL_DIR/$BIN"
chmod 0755 "$INSTALL_DIR/$BIN"
echo "installed $INSTALL_DIR/$BIN"
