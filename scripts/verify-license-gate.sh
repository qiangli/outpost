#!/usr/bin/env bash
# Check the shipped Outpost graph with the pinned bashy licensegate source.
set -euo pipefail

source_commit=a68c998759bcf731fa7cd1c692a64f27a767f71e
source_sha256=910e78f82fa0c9e12161b7e5a3541728fd1c185734f0fad4794d448ae3a14b35
root=$(cd "$(dirname "$0")/.." && pwd)
temp_dir=$(mktemp -d)
trap 'rm -rf "$temp_dir"' EXIT

curl -fsSL --retry 3 \
  "https://raw.githubusercontent.com/qiangli/bashy/${source_commit}/tools/licensegate/main.go" \
  -o "$temp_dir/licensegate.go"
printf '%s  %s\n' "$source_sha256" "$temp_dir/licensegate.go" | shasum -a 256 -c -
go build -o "$temp_dir/licensegate" "$temp_dir/licensegate.go"

cd "$root"
GOWORK=off "$temp_dir/licensegate" -targets ./cmd/outpost -tags fb_archives -scan-dir .
