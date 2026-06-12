#!/usr/bin/env bash
# Ensure the sibling-path replace targets in go.mod (../sh,
# ../coreutils) exist on
# disk, by cloning the pinned commit from each upstream repo if the
# sibling is missing.
#
# Why: outpost lives in two contexts.
#   1. Inside the dhnt umbrella, ../sh is already mounted as a
#      submodule (dhnt/sh). The script detects it and leaves it
#      alone — one shared copy across every consumer in the
#      umbrella (outpost, ycode, …).
#   2. As a standalone clone (CI runner, contributor checkout, or a
#      user self-rebuilding via `outpost git clone … && outpost shell
#      ./scripts/build.sh`), the sibling doesn't exist. The script
#      clones it into ../sh at the SHA in .sibling-pins.
#
# Uses `outpost git` when available (zero system-git dependency, the
# Windows-friendly self-rebuild path) and falls back to system `git` on
# CI runners and machines that don't have outpost installed yet.
#
# Idempotent. Safe to re-run.
set -euo pipefail

cd "$(dirname "$0")/.."
root=$(pwd)
pins=$root/.sibling-pins

if [ ! -f "$pins" ]; then
    echo "bootstrap-siblings: missing $pins" >&2
    exit 1
fi

# pick_git_cli: the only shared helper that needs to be aware of both
# clients. Probes that `outpost git` actually accepts `git` as a
# subcommand (older installed outposts may not). Falls back to system
# `git` when outpost can't do it, and errors when neither is available.
pick_git_cli() {
    if command -v outpost >/dev/null 2>&1; then
        if outpost git --help >/dev/null 2>&1; then
            echo outpost
            return 0
        fi
    fi
    if command -v git >/dev/null 2>&1; then
        echo system
        return 0
    fi
    echo "bootstrap-siblings: neither 'outpost git' nor system 'git' available" >&2
    return 1
}

git_cli=$(pick_git_cli)

git_clone_quiet() {
    # args: url target
    if [ "$git_cli" = outpost ]; then
        outpost git clone --quiet "$1" "$2"
    else
        git clone --quiet "$1" "$2"
    fi
}

git_checkout_quiet() {
    # args: target sha
    if [ "$git_cli" = outpost ]; then
        (cd "$1" && outpost git checkout "$2") >/dev/null
    else
        git -C "$1" checkout --quiet "$2"
    fi
}

git_short_head() {
    # args: target
    if [ "$git_cli" = outpost ]; then
        (cd "$1" && outpost git rev-parse --short HEAD)
    else
        (cd "$1" && git rev-parse --short HEAD)
    fi
}

# repo URL per dep name; if you add a new sibling, append here.
repo_url() {
    case "$1" in
        sh) echo "https://github.com/qiangli/sh.git" ;;
        coreutils) echo "https://github.com/qiangli/coreutils.git" ;;
        *) echo "bootstrap-siblings: no repo URL for '$1'" >&2; return 1 ;;
    esac
}

while IFS= read -r line; do
    case "$line" in
        ''|'#'*) continue ;;
    esac
    name=${line%%=*}
    sha=${line#*=}
    if [ -z "$name" ] || [ -z "$sha" ] || [ "$name" = "$sha" ]; then
        echo "bootstrap-siblings: malformed line: $line" >&2
        exit 1
    fi

    target=$root/../$name
    if [ -e "$target/.git" ]; then
        echo "bootstrap-siblings: $name -> $(git_short_head "$target") (already present, leaving alone)"
        continue
    fi

    url=$(repo_url "$name")
    echo "bootstrap-siblings: cloning $url -> $target @ ${sha:0:12}"
    git_clone_quiet "$url" "$target"
    git_checkout_quiet "$target" "$sha"
done < "$pins"
