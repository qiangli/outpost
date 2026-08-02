#!/usr/bin/env bash
# script/dks-peer-image-reach.sh — verification runner for peer image
# distribution and reach ("recipes, not blobs", docs/peer-dks-image-distribution.md).
#
# Emits machine-readable lines:
#     CHECK <name> PASS|FAIL|BLOCKED <detail>
# and a final summary.
#
# Exit status contract:
#     0  OK           — at least one check PASSed and none FAILed
#     1  FAIL         — at least one check FAILed
#     2  INCONCLUSIVE — nothing was proven (0 PASSed). Absence of evidence is
#                       NEVER success (evidence invariant).
#
# Usage:
#   PEER_IMAGE_REACH_LIB_ONLY=1 source script/dks-peer-image-reach.sh
#   ./script/dks-peer-image-reach.sh

set -eo pipefail

REACH_PASS_COUNT=0
REACH_FAIL_COUNT=0
REACH_BLOCKED_COUNT=0
REACH_RESULTS=()

reach_record() {
    local name="$1" status="$2"
    local detail="${3:-}"
    case "$status" in
        PASS)    REACH_PASS_COUNT=$((REACH_PASS_COUNT + 1)) ;;
        FAIL)    REACH_FAIL_COUNT=$((REACH_FAIL_COUNT + 1)) ;;
        BLOCKED) REACH_BLOCKED_COUNT=$((REACH_BLOCKED_COUNT + 1)) ;;
        *)
            REACH_FAIL_COUNT=$((REACH_FAIL_COUNT + 1))
            status="FAIL"
            detail="harness error: unknown status; $detail"
            ;;
    esac
    REACH_RESULTS+=("CHECK $name $status $detail")
    echo "CHECK $name $status $detail"
}

reach_summary() {
    echo "SUMMARY pass=$REACH_PASS_COUNT fail=$REACH_FAIL_COUNT blocked=$REACH_BLOCKED_COUNT"
    if [ "$REACH_FAIL_COUNT" -gt 0 ]; then
        echo "RESULT FAIL"
        return 1
    fi
    if [ "$REACH_PASS_COUNT" -eq 0 ]; then
        echo "RESULT INCONCLUSIVE"
        return 2
    fi
    echo "RESULT PASS"
    return 0
}

# reach_verify_distinct_nodes <min_required> <space_separated_nodes>
# Verifies that node targets are DISTINCT node identities (node names with backend
# discriminators, never plain host labels), non-empty, and at least min_required.
reach_verify_distinct_nodes() {
    local min_required="$1"
    local node_list="$2"
    local seen_nodes=" "
    local distinct_count=0
    local node

    if [ -z "$node_list" ]; then
        reach_record "distinct-targets" "FAIL" "no node targets supplied (minimum $min_required required)"
        return 1
    fi

    for node in $node_list; do
        if [ -z "$node" ]; then
            reach_record "distinct-targets" "FAIL" "unnamed node target cannot be identified"
            return 1
        fi
        if [[ "$seen_nodes" == *" $node "* ]]; then
            reach_record "distinct-targets" "FAIL" "duplicate node target \"$node\" listed (node names must name distinct backends)"
            return 1
        fi
        seen_nodes="$seen_nodes$node "
        distinct_count=$((distinct_count + 1))
    done

    if [ "$distinct_count" -lt "$min_required" ]; then
        reach_record "distinct-targets" "FAIL" "$distinct_count distinct node(s) available, $min_required required"
        return 1
    fi

    reach_record "distinct-targets" "PASS" "$distinct_count distinct node target(s) verified (minimum $min_required)"
    return 0
}

# reach_eval_report <ch_spec> <rep_spec>
# ch_spec:  "ch_node|ch_ref|ch_recipe|ch_nonce"
# rep_spec: "r_node|r_ref|r_recipe|r_nonce|r_state|r_content|r_prov"
reach_eval_report() {
    local ch_spec="$1" rep_spec="$2"

    local ch_node="${ch_spec%%|*}"
    local rest="${ch_spec#*|}"
    local ch_ref="${rest%%|*}"
    rest="${rest#*|}"
    local ch_recipe="${rest%%|*}"
    local ch_nonce="${rest#*|}"

    local r_node="${rep_spec%%|*}"
    rest="${rep_spec#*|}"
    local r_ref="${rest%%|*}"
    rest="${rest#*|}"
    local r_recipe="${rest%%|*}"
    rest="${rest#*|}"
    local r_nonce="${rest%%|*}"
    rest="${rest#*|}"
    local r_state="${rest%%|*}"
    rest="${rest#*|}"
    local r_content="${rest%%|*}"
    local r_prov="${rest#*|}"

    # Check 1: Foreign node / identity mismatch
    if [ "$r_node" != "$ch_node" ]; then
        echo "REJECT foreign node: report from \"$r_node\" does not match challenged node \"$ch_node\""
        return 1
    fi

    # Check 2: Nonce / Challenge identity binding
    if [ -z "$r_nonce" ] || [ "$r_nonce" != "$ch_nonce" ]; then
        echo "REJECT foreign evidence: nonce \"$r_nonce\" does not match challenge nonce \"$ch_nonce\" for node \"$ch_node\""
        return 1
    fi

    if [ "$r_ref" != "$ch_ref" ]; then
        echo "REJECT identity mismatch: report ref \"$r_ref\" != challenge ref \"$ch_ref\""
        return 1
    fi

    if [ "$r_recipe" != "$ch_recipe" ]; then
        echo "REJECT identity mismatch: report recipe \"$r_recipe\" != challenge recipe \"$ch_recipe\""
        return 1
    fi

    # Check 3: State & containerd digest validation
    case "$r_state" in
        resident)
            ;;
        absent)
            echo "FAIL image is absent from node containerd"
            return 1
            ;;
        unknown)
            echo "FAIL could not determine resident containerd digest"
            return 1
            ;;
        *)
            echo "FAIL unknown digest state \"$r_state\""
            return 1
            ;;
    esac

    if [[ "$r_content" != sha256:* ]] || [ "${#r_content}" -ne 71 ]; then
        echo "FAIL invalid content digest format \"$r_content\""
        return 1
    fi

    if [ -z "$r_prov" ]; then
        echo "FAIL no local provenance recorded for resident image"
        return 1
    fi

    # Check 4: Correlate ACTUAL containerd content digest with provenance
    if [ "$r_content" != "$r_prov" ]; then
        echo "FAIL digest mismatch: resident containerd digest \"$r_content\" != recorded provenance \"$r_prov\""
        return 1
    fi

    echo "ACCEPTED node=\"$r_node\" ref=\"$r_ref\" digest=\"$r_content\""
    return 0
}

# Main execution if not sourced as library
if [ "${PEER_IMAGE_REACH_LIB_ONLY:-0}" -ne 1 ] && [ "${1:-}" != "--lib-only" ]; then
    echo "peer-image-reach runner (recipes, not blobs)"
    if [ $# -eq 0 ]; then
        reach_record "evidence-invariant" "BLOCKED" "no input arguments provided; run offline test suite script/dks-peer-image-reach_test.sh"
    fi
    reach_summary
    exit $?
fi
