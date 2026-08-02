#!/usr/bin/env bash
# dks-peer-bundle-apply.sh — operator entry point that installs an
# app/bundle manifest set against a PEER-HOSTED DKS control plane, using
# ONLY a kubeconfig path. No cloudbox: no access_token, no /api/v1 call,
# no overlay credential fetch anywhere on this path.
#
# It is a thin front over the Go package internal/agent/bundleapply (run
# via `go run ./internal/agent/bundleapply/cmd`). All Kubernetes work —
# ordering (Namespace before namespaced objects), idempotent server-side
# apply, and the bounded readiness wait — lives in the package; this
# script owns argument handling, the peer/cloudbox kubeconfig distinction,
# and loud failure on anything unsupported or absent.
#
# The two kubeconfigs are NEVER conflated:
#     peer control plane : ~/.kube/outpost-control-plane/k3s.yaml
#     cloudbox plane     : ~/.kube/outpost.yaml
# There is no default cluster: you pass --kubeconfig or --peer explicitly.
#
# Evidence invariant (docs/fleet-evidence-invariant.md): a missing
# kubeconfig, a missing bundle, or an unsupported flag FAILS loudly. No
# success is ever reached by the absence of evidence.
#
# Usage:
#   script/dks-peer-bundle-apply.sh --peer --bundle ./bundle
#   script/dks-peer-bundle-apply.sh --kubeconfig /path/k3s.yaml --bundle ./app.yaml [--timeout 5m] [--poll 2s]
#
# Flags:
#   --peer                use the peer control-plane kubeconfig ($DKS_PEER_KUBECONFIG)
#   --kubeconfig PATH     explicit kubeconfig path (mutually exclusive with --peer)
#   --bundle PATH         manifest file or directory of manifests (required)
#   --timeout DUR         bounded readiness wait (default 5m)
#   --poll DUR            readiness poll interval (default 2s)
#   -h | --help           this help
#
# Env (mostly for tests — never needed in normal operation):
#   DKS_PEER_KUBECONFIG      peer kubeconfig path (default ~/.kube/outpost-control-plane/k3s.yaml)
#   DKS_CLOUDBOX_KUBECONFIG  cloudbox kubeconfig path (default ~/.kube/outpost.yaml) — used only to REJECT it
#   DKS_BUNDLE_APPLY_CMD     override the runner command (default: go run ./internal/agent/bundleapply/cmd)
#
# Sourcing with DKS_BUNDLE_APPLY_LIB_ONLY=1 loads the pure helpers without
# running — this is what the offline tests exercise.

set -euo pipefail

: "${DKS_PEER_KUBECONFIG:=${HOME}/.kube/outpost-control-plane/k3s.yaml}"
: "${DKS_CLOUDBOX_KUBECONFIG:=${HOME}/.kube/outpost.yaml}"

die() { echo "dks-peer-bundle-apply: $*" >&2; exit 2; }

usage() {
    sed -n '2,60p' "$0" | sed 's/^# \{0,1\}//'
}

# expand_tilde <path> — expand a leading ~ / ~/ to $HOME. Pure; no I/O.
expand_tilde() {
    local p="$1"
    case "$p" in
        "~")   printf '%s\n' "$HOME" ;;
        "~/"*) printf '%s\n' "$HOME/${p#\~/}" ;;
        *)     printf '%s\n' "$p" ;;
    esac
}

# resolve_kubeconfig <use_peer> <explicit> — decide the kubeconfig path,
# enforcing: exactly one source, and never the cloudbox file. Echoes the
# resolved path on success; on a policy violation it prints to stderr and
# returns non-zero (the caller turns that into exit 2). No default cluster.
resolve_kubeconfig() {
    local use_peer="$1" explicit="$2" path=""
    if [ "$use_peer" = "1" ] && [ -n "$explicit" ]; then
        echo "--peer and --kubeconfig are mutually exclusive" >&2; return 1
    fi
    if [ "$use_peer" = "1" ]; then
        path="$DKS_PEER_KUBECONFIG"
    elif [ -n "$explicit" ]; then
        path="$(expand_tilde "$explicit")"
    else
        echo "no kubeconfig: pass --peer or --kubeconfig PATH (there is no default cluster)" >&2
        return 1
    fi
    # Never apply a peer bundle against the cloudbox kubeconfig.
    local cloudbox; cloudbox="$(expand_tilde "$DKS_CLOUDBOX_KUBECONFIG")"
    if [ "$path" = "$cloudbox" ]; then
        echo "refusing to apply a PEER bundle against the cloudbox kubeconfig ($path) — the peer and cloudbox planes are never conflated" >&2
        return 1
    fi
    printf '%s\n' "$path"
}

# When sourced for tests, stop here: helpers are defined, nothing runs.
if [ "${DKS_BUNDLE_APPLY_LIB_ONLY:-0}" = "1" ]; then
    return 0 2>/dev/null || exit 0
fi

main() {
    local use_peer=0 kubeconfig="" bundle="" timeout="5m" poll="2s"
    while [ $# -gt 0 ]; do
        case "$1" in
            --peer)          use_peer=1; shift ;;
            --kubeconfig)    kubeconfig="${2:-}"; [ -n "$kubeconfig" ] || die "--kubeconfig requires a value"; shift 2 ;;
            --kubeconfig=*)  kubeconfig="${1#*=}"; shift ;;
            --bundle)        bundle="${2:-}"; [ -n "$bundle" ] || die "--bundle requires a value"; shift 2 ;;
            --bundle=*)      bundle="${1#*=}"; shift ;;
            --timeout)       timeout="${2:-}"; [ -n "$timeout" ] || die "--timeout requires a value"; shift 2 ;;
            --timeout=*)     timeout="${1#*=}"; shift ;;
            --poll)          poll="${2:-}"; [ -n "$poll" ] || die "--poll requires a value"; shift 2 ;;
            --poll=*)        poll="${1#*=}"; shift ;;
            -h|--help)       usage; exit 0 ;;
            --)              shift; break ;;
            -*)              die "unknown flag: $1 (unsupported flags fail loudly; no silent fallback)" ;;
            *)               die "unexpected argument: $1" ;;
        esac
    done
    [ $# -eq 0 ] || die "unexpected trailing arguments: $*"

    [ -n "$bundle" ] || die "--bundle is required"

    local kc
    kc="$(resolve_kubeconfig "$use_peer" "$kubeconfig")" || exit 2

    # Evidence invariant: the kubeconfig and the bundle must actually exist.
    [ -f "$kc" ] || die "kubeconfig not found: $kc"
    [ -e "$bundle" ] || die "bundle path not found: $bundle"

    echo "dks-peer-bundle-apply: kubeconfig=$kc bundle=$bundle timeout=$timeout poll=$poll" >&2

    # Hand off to the Go runner. Word-split the override intentionally so a
    # test can inject a stub like "bash stub.sh".
    local -a runner
    if [ -n "${DKS_BUNDLE_APPLY_CMD:-}" ]; then
        # shellcheck disable=SC2206
        runner=(${DKS_BUNDLE_APPLY_CMD})
    else
        runner=(go run ./internal/agent/bundleapply/cmd)
    fi

    exec "${runner[@]}" \
        --kubeconfig "$kc" \
        --bundle "$bundle" \
        --timeout "$timeout" \
        --poll "$poll"
}

main "$@"
