#!/usr/bin/env bash
# scripts/clean.sh — remove build artifacts. Replaces `make clean`.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
cd "$ROOT"
# shellcheck source=lib.sh
. "$ROOT/scripts/lib.sh"

rm -rf "$OUT_DIR"
rm -f "$BIN" ./*.test ./*.out
