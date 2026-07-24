#!/usr/bin/env bash
# scripts/lib.sh — shared vars + helpers for outpost's build scripts.
# Sourced by build.sh / build-all.sh / install-bin.sh / clean.sh / tidy.sh.
# Not directly executable.
#
# Replaces the Makefile's variable block + LDFLAGS computation. Same
# inputs, same outputs — `outpost version --json` reports the same
# commit/dirty/releaseTag fields either way.

# Don't `set -e` here — callers source us, and a failure in the bootstrap
# helpers below (e.g. detecting `go` on PATH) shouldn't abort the caller's
# own setup. Each call-site uses `|| return 1` where it matters.

BIN="${BIN:-outpost}"
PKG="${PKG:-./cmd/outpost}"
OUT_DIR="${OUT_DIR:-bin}"
INSTALL_DIR="${INSTALL_DIR:-${DHNT_BIN_DIR:-$HOME/.local/bin}}"

# Cross-build matrix. Mirrors .github/workflows/release.yml so build-all.sh
# produces the same set of artifacts the release flow uploads to GH.
PLATFORMS="${PLATFORMS:-darwin-amd64 darwin-arm64 linux-amd64 linux-arm64 windows-amd64 windows-arm64}"

# RELEASE_TAG, when non-empty, additionally stamps a semver tag onto the
# binary. Set by the GitHub Actions release workflow from the triggering
# git tag (e.g. RELEASE_TAG=v0.2.0). Local builds leave it empty and the
# binary surfaces only the commit short-sha.
RELEASE_TAG="${RELEASE_TAG:-}"

# repo_root: project root regardless of where the caller cd'd from.
# Scripts live in <root>/scripts, so this is the parent of $0's directory.
repo_root() {
    # $1 = the calling script's path (typically "$0")
    (cd "$(dirname "$1")/.." && pwd)
}

# compute_commit: short-SHA of HEAD (7 chars). Tries `outpost git` first
# (the self-rebuild path with no system git installed), then falls back
# to system `git`. Echoes empty when neither works — that's load-bearing,
# because an empty ldCommit makes the binary fall through to
# runtime/debug.ReadBuildInfo, which walks UP past the submodule
# boundary into the dhnt umbrella's HEAD. The Makefile dodged this with
# explicit `git rev-parse --short HEAD`; we do the same here.
#
# An installed outpost that pre-dates the `git` subcommand returns
# non-zero on the probe — the fallback handles it.
compute_commit() {
    local sha=""
    if command -v outpost >/dev/null 2>&1; then
        sha=$(outpost git rev-parse --short HEAD 2>/dev/null || true)
        if [ -n "$sha" ]; then
            echo "$sha"
            return 0
        fi
    fi
    if command -v git >/dev/null 2>&1; then
        sha=$(git rev-parse --short HEAD 2>/dev/null || true)
        if [ -n "$sha" ]; then
            echo "$sha"
            return 0
        fi
    fi
    echo ""
}

# compute_dirty: "true" if working tree has uncommitted changes, else
# "false". Matches the Makefile's `git diff --quiet && echo false || echo
# true`. Same outpost-then-git probe-with-fallback as compute_commit.
compute_dirty() {
    if command -v outpost >/dev/null 2>&1; then
        local out
        out=$(outpost git rev-parse --is-dirty 2>/dev/null || true)
        # --is-dirty exits non-zero when dirty, so we ignore the exit
        # code and read stdout. "true"/"false" both indicate the probe
        # actually ran on an outpost that has the subcommand.
        case "$out" in
            true|false) echo "$out"; return 0 ;;
        esac
    fi
    if command -v git >/dev/null 2>&1; then
        if git diff --quiet 2>/dev/null; then
            echo "false"
        else
            echo "true"
        fi
        return 0
    fi
    echo "false"
}

# compute_ldflags: assembles the -ldflags string injected into the binary.
# Produces the same `-X github.com/qiangli/outpost/internal/agent.ld…=…`
# triple the Makefile produced, including the optional releaseTag.
compute_ldflags() {
    local commit dirty
    commit=$(compute_commit)
    dirty=$(compute_dirty)
    local flags="-X github.com/qiangli/outpost/internal/agent.ldCommit=${commit} -X github.com/qiangli/outpost/internal/agent.ldDirty=${dirty}"
    if [ -n "$RELEASE_TAG" ]; then
        flags="${flags} -X github.com/qiangli/outpost/internal/agent.releaseTag=${RELEASE_TAG}"
    fi
    echo "$flags"
}
