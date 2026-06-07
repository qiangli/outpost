#!/usr/bin/env bash
# scripts/build-all.sh — cross-compile outpost for every release platform.
# Replaces `make build-all`.
#
# Mirrors the matrix in .github/workflows/release.yml. Each platform
# delegates to build.sh with GOOS/GOARCH set, so the per-platform ldflags +
# trimpath logic stays in one place.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
cd "$ROOT"
# shellcheck source=lib.sh
. "$ROOT/scripts/lib.sh"

mkdir -p "$OUT_DIR"

for p in $PLATFORMS; do
    os=${p%-*}
    arch=${p##*-}
    out="$OUT_DIR/$BIN-$p"
    if [ "$os" = "windows" ]; then
        out="${out}.exe"
    fi
    echo "  → $out"
    # OUT_DIR + BIN are passed through env so build.sh produces the
    # per-platform filename instead of the default ./bin/outpost.
    GOOS="$os" GOARCH="$arch" CGO_ENABLED=0 \
        BIN="$BIN-$p" \
        "$ROOT/scripts/build.sh"
done

ls -lh "$OUT_DIR/$BIN-"*
