#!/usr/bin/env bash
# scripts/build.sh — build outpost for the current platform into ./bin.
# Replaces `make build`.
#
# Honors $GOOS, $GOARCH, $CGO_ENABLED from the environment so build-all.sh
# can reuse this script across the cross-compile matrix.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
cd "$ROOT"
# shellcheck source=lib.sh
. "$ROOT/scripts/lib.sh"

mkdir -p "$OUT_DIR"

OUT="$OUT_DIR/$BIN"
# Append .exe on windows so cross-builds (build-all.sh sets GOOS=windows)
# land on the expected filename without each caller knowing.
if [ "${GOOS:-}" = "windows" ]; then
    OUT="${OUT}.exe"
fi

LDFLAGS=$(compute_ldflags)

# CGO_ENABLED=0 by default — outpost has no cgo deps and disabling cgo
# is what makes cross-compile work everywhere. Callers can override.
export CGO_ENABLED="${CGO_ENABLED:-0}"

go build -ldflags "$LDFLAGS" -trimpath -o "$OUT" "$PKG"
