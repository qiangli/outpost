#!/usr/bin/env bash
# scripts/tidy.sh — go mod tidy + go fmt + go vet. Replaces `make tidy`.
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/.." && pwd)
cd "$ROOT"

go mod tidy
go fmt ./...
go vet ./...
