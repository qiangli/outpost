#!/usr/bin/env bash
# Developer-side deploy: build the outpost binary locally and ship it
# to one or more paired hosts via scp. Complements scripts/install.sh,
# which is the user-facing path that pulls a release from GitHub.
#
# Why a separate script: this path skips the GitHub release step so
# unreleased / branch-tip code can land on a host quickly. Mainly used
# during phase-rollout to verify a binary on real hardware before
# tagging a release.
#
# The macOS arm64 quirk this exists for: `scp` of a Go-built Mach-O
# preserves file content but the receiving filesystem's per-file
# `AppleDouble` / signature-cache state ends up rejecting the binary
# with `Killed: 9` on first exec — even though the ad-hoc signature
# embedded in the binary itself is intact. Re-running `codesign -s -`
# on the destination re-stamps the local signature DB and unblocks
# execution. This script always runs that step after scp.
#
# Usage:
#   scripts/scp-deploy.sh <host> [<host>...]
#   scripts/scp-deploy.sh --restart <host>            # also `outpost restart` after
#   scripts/scp-deploy.sh --bin /path/to/outpost <h>  # skip build, ship a prebuilt
#   GOOS=linux GOARCH=amd64 scripts/scp-deploy.sh <host>
#
# Each <host> is an ssh alias (resolved via ~/.ssh/config). The
# remote path is ${REMOTE_BIN_DIR:-~/bin}/outpost; the previous binary
# is preserved as outpost.prev for atomic rollback (`mv ~/bin/outpost.prev
# ~/bin/outpost` on the host).

set -euo pipefail

REMOTE_BIN_DIR="${REMOTE_BIN_DIR:-bin}"   # relative to remote $HOME
REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"

bold=""; dim=""; red=""; green=""; yellow=""; reset=""
if [ -t 1 ]; then
    bold=$(printf '\033[1m')
    dim=$(printf '\033[2m')
    red=$(printf '\033[31m')
    green=$(printf '\033[32m')
    yellow=$(printf '\033[33m')
    reset=$(printf '\033[0m')
fi

log()  { printf '%s==>%s %s\n' "${bold}" "${reset}" "$*"; }
warn() { printf '%s!!%s  %s\n' "${yellow}" "${reset}" "$*" >&2; }
err()  { printf '%sxx%s  %s\n' "${red}" "${reset}" "$*" >&2; }

usage() {
    sed -n '/^# Usage:/,/^$/p' "$0" | sed 's/^# \{0,1\}//'
    exit 2
}

RESTART=0
BIN=""
HOSTS=()

while [ $# -gt 0 ]; do
    case "$1" in
        --restart) RESTART=1; shift;;
        --bin) BIN="$2"; shift 2;;
        -h|--help) usage;;
        --) shift; while [ $# -gt 0 ]; do HOSTS+=("$1"); shift; done;;
        -*) err "unknown flag: $1"; usage;;
        *) HOSTS+=("$1"); shift;;
    esac
done

[ "${#HOSTS[@]}" -gt 0 ] || { err "at least one <host> required"; usage; }

# Build (or accept --bin) ----------------------------------------------
if [ -z "$BIN" ]; then
    BIN="$(mktemp -t outpost-deploy.XXXXXX)"
    log "build outpost → ${BIN}  (GOOS=${GOOS:-host}, GOARCH=${GOARCH:-host})"
    (cd "$REPO_ROOT" && GOTOOLCHAIN=auto go build -o "$BIN" ./cmd/outpost)
else
    [ -x "$BIN" ] || { err "--bin ${BIN} not executable"; exit 1; }
    log "using prebuilt ${BIN}"
fi

# Stamp an ad-hoc signature on the build artifact too. Idempotent;
# costs nothing on Linux (`codesign` is macOS-only — skip there).
if [ "$(uname -s)" = "Darwin" ]; then
    codesign -s - "$BIN" 2>/dev/null || true
fi

LOCAL_VERSION="$("$BIN" version 2>/dev/null || echo unknown)"
log "local binary version: ${dim}${LOCAL_VERSION}${reset}"

# Per-host deploy ------------------------------------------------------
deploy_one() {
    local host="$1"
    log "→ ${bold}${host}${reset}"

    # Probe the remote OS so we know whether to re-sign.
    local remote_os
    remote_os=$(ssh -o ConnectTimeout=10 -o BatchMode=yes "$host" 'uname -s' 2>/dev/null || true)
    if [ -z "$remote_os" ]; then
        err "  ${host}: cannot ssh (check ~/.ssh/config and host reachability); skipping"
        return 1
    fi

    # Stage to /tmp first so we never overwrite a running binary
    # in-place — the on-disk replace is atomic via mv.
    log "  scp → /tmp/outpost.new"
    scp -q "$BIN" "${host}:/tmp/outpost.new"

    # Atomic install with prev-backup for rollback, then re-sign on
    # macOS so the local signature DB accepts it. The codesign step is
    # the load-bearing part on arm64 — without it, the binary launches
    # to SIGKILL even though the in-binary signature is intact.
    log "  install → ~/${REMOTE_BIN_DIR}/outpost (backup → outpost.prev)"
    local resign=""
    case "$remote_os" in
        Darwin) resign="codesign -s - ~/${REMOTE_BIN_DIR}/outpost && " ;;
        *)      resign="" ;;
    esac

    ssh -o BatchMode=yes "$host" "
        set -eu
        mkdir -p ~/${REMOTE_BIN_DIR}
        if [ -f ~/${REMOTE_BIN_DIR}/outpost ]; then
            cp ~/${REMOTE_BIN_DIR}/outpost ~/${REMOTE_BIN_DIR}/outpost.prev
        fi
        mv /tmp/outpost.new ~/${REMOTE_BIN_DIR}/outpost
        chmod +x ~/${REMOTE_BIN_DIR}/outpost
        ${resign}~/${REMOTE_BIN_DIR}/outpost version
    "

    if [ "$RESTART" = "1" ]; then
        log "  outpost restart"
        ssh -o BatchMode=yes "$host" "~/${REMOTE_BIN_DIR}/outpost restart" || \
            warn "  ${host}: restart failed (daemon may need manual start)"
    fi

    printf '%s✓%s %s\n' "${green}" "${reset}" "${host}: deployed"
}

FAILED=()
for host in "${HOSTS[@]}"; do
    if ! deploy_one "$host"; then
        FAILED+=("$host")
    fi
done

if [ "${#FAILED[@]}" -gt 0 ]; then
    err "failed: ${FAILED[*]}"
    exit 1
fi

log "all done."
