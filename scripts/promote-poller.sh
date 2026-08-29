#!/usr/bin/env bash
# promote-poller — create the bare release tag once the configured QA refs exist.
#
# This is the final automated baton in the tag-driven release pipeline:
#   vX.Y.Z-dev pre-release -> refs/qa/<ver>/<os> -> bare vX.Y.Z tag
#
# The bare tag push is what fires .github/workflows/promote.yml. This script does
# not publish bytes itself; promote.yml still performs the byte-identical release
# promotion after re-checking the same required-OS gate.
set -uo pipefail
export PATH="$HOME/bin:$PATH"

REPO="${REPO:-${OUTPOST_REPO:-qiangli/outpost}}"
REQUIRED_OS="${REQUIRED_OS:-windows}"
INTERVAL="${PROMOTE_POLL_INTERVAL:-300}"
ONCE="${PROMOTE_POLL_ONCE:-}"
export OTEL_SERVICE_NAME="${OTEL_SERVICE_NAME:-outpost-promote-poller}"

gh_ok(){ bashy gh auth token >/dev/null 2>&1 || [ -n "${GITHUB_TOKEN:-}" ] || eval "$(bashy secrets env 2>/dev/null)"; [ -n "${GITHUB_TOKEN:-}$(bashy gh auth token 2>/dev/null)" ]; }

newest_dev(){
  bashy git ls-remote --tags "https://github.com/$REPO.git" 2>/dev/null | awk -F/ '
    /refs\/tags\/v[0-9]+\.[0-9]+\.[0-9]+-dev$/ {
      t=$NF; v=t; sub(/-dev$/,"",v); sub(/^v/,"",v); split(v,a,".")
      k=a[1]*1000000+a[2]*1000+a[3]; if (k>m){m=k; b=t}
    } END{ if (b) print b }'
}

tag_sha(){
  local tag="$1" sha
  sha=$(bashy git ls-remote --tags "https://github.com/$REPO.git" "refs/tags/$tag^{}" | awk '{print $1}' | head -1)
  [ -n "$sha" ] || sha=$(bashy git ls-remote --tags "https://github.com/$REPO.git" "refs/tags/$tag" | awk '{print $1}' | head -1)
  echo "$sha"
}

pass(){
  local dev ver missing os sha
  dev=$(newest_dev)
  [ -n "$dev" ] || { echo "[promote] no -dev tag yet"; return 0; }
  ver="${dev%-dev}"

  if bashy gh api "/repos/$REPO/git/ref/tags/$ver" >/dev/null 2>&1; then
    echo "[promote] $ver already tagged"; return 0
  fi

  missing=""
  for os in $REQUIRED_OS; do
    if bashy gh api "/repos/$REPO/git/refs/qa/$ver/$os" >/dev/null 2>&1; then
      echo "  qa-$os: green"
    else
      missing="$missing $os"
    fi
  done
  if [ -n "$missing" ]; then
    echo "[promote] $ver blocked on required QA refs:$missing"
    return 0
  fi

  sha=$(tag_sha "$dev")
  [ -n "$sha" ] || { echo "[promote] FAIL: could not resolve $dev"; return 1; }
  if bashy gh api -X POST "/repos/$REPO/git/refs" -f "ref=refs/tags/$ver" -f "sha=$sha" >/dev/null 2>&1; then
    echo "[promote] TAGGED refs/tags/$ver at $sha; promote.yml should fire"
  else
    echo "[promote] FAIL: could not create refs/tags/$ver"
    return 1
  fi
}

gh_ok || { echo "promote-poller: no GitHub token (set GITHUB_TOKEN / bashy gh auth login / bashy secrets)"; exit 2; }
if [ -n "$ONCE" ]; then pass; exit $?; fi
while true; do pass; sleep "$INTERVAL"; done
