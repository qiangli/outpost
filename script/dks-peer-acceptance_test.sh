#!/usr/bin/env bash
# Offline tests for script/dks-peer-acceptance.sh's own logic.
# Needs NO cluster, NO kubectl, NO network. Run: bash script/dks-peer-acceptance_test.sh
set -uo pipefail

# $0 rather than BASH_SOURCE: this file is always executed, never sourced, and
# BASH_SOURCE is not populated by every bash-compatible runner (e.g. bashy).
HERE="$(cd "$(dirname "$0")" && pwd)"
HARNESS="$HERE/dks-peer-acceptance.sh"

T_PASS=0; T_FAIL=0
ok()   { T_PASS=$((T_PASS+1)); echo "ok   - $1"; }
bad()  { T_FAIL=$((T_FAIL+1)); echo "FAIL - $1"; [ -n "${2:-}" ] && echo "       $2"; }
is()   { if [ "$2" = "$3" ]; then ok "$1"; else bad "$1" "want [$3] got [$2]"; fi; }

# Load the pure helpers only.
DKS_LIB_ONLY=1 . "$HARNESS"

reset() { DKS_PASS_COUNT=0; DKS_FAIL_COUNT=0; DKS_BLOCKED_COUNT=0; DKS_RESULTS=(); }

# --- dks_distinct_cidrs -----------------------------------------------------
out="$(printf 'a 10.42.0.0/24\nb 10.42.1.0/24\nc 10.42.2.0/24\n' | dks_distinct_cidrs)"; rc=$?
is "distinct cidrs -> rc 0" "$rc" "0"
case "$out" in *"3 distinct"*) ok "distinct cidrs -> reports count" ;; *) bad "distinct cidrs reason" "$out" ;; esac

out="$(printf 'a 10.42.0.0/24\nb 10.42.0.0/24\n' | dks_distinct_cidrs)"; rc=$?
is "duplicate cidrs -> rc 1" "$rc" "1"
case "$out" in *duplicate*b=10.42.0.0/24*) ok "duplicate cidrs -> names the node" ;; *) bad "duplicate reason" "$out" ;; esac

out="$(printf 'a 10.42.0.0/24\nb <none>\n' | dks_distinct_cidrs)"; rc=$?
is "empty cidr -> rc 1" "$rc" "1"
case "$out" in *"empty podCIDR on: b"*) ok "empty cidr -> names the node" ;; *) bad "empty reason" "$out" ;; esac

out="$(printf 'a 10.42.0.0/24\nb\n' | dks_distinct_cidrs)"; rc=$?
is "missing cidr field -> rc 1" "$rc" "1"

out="$(printf '' | dks_distinct_cidrs)"; rc=$?
is "no nodes -> rc 1 (never silently ok)" "$rc" "1"
case "$out" in *"no nodes"*) ok "no nodes -> explains" ;; *) bad "no-nodes reason" "$out" ;; esac

# A three-node set where only the 3rd collides must still fail.
out="$(printf 'a 10.42.0.0/24\nb 10.42.1.0/24\nc 10.42.1.0/24\n' | dks_distinct_cidrs)"; rc=$?
is "late duplicate -> rc 1" "$rc" "1"

# --- dks_is_tailnet_ip ------------------------------------------------------
dks_is_tailnet_ip 100.64.0.1  && ok "100.64.0.1 is tailnet"       || bad "100.64.0.1 is tailnet"
dks_is_tailnet_ip 100.127.255.254 && ok "100.127.x is tailnet"    || bad "100.127.x is tailnet"
dks_is_tailnet_ip 100.63.0.1  && bad "100.63.0.1 must NOT be tailnet" || ok "100.63.0.1 not tailnet"
dks_is_tailnet_ip 100.128.0.1 && bad "100.128.0.1 must NOT be tailnet" || ok "100.128.0.1 not tailnet"
dks_is_tailnet_ip 10.88.0.130 && bad "10.88.0.130 must NOT be tailnet" || ok "podman bridge ip not tailnet"
dks_is_tailnet_ip ""          && bad "empty must NOT be tailnet"  || ok "empty not tailnet"
dks_is_tailnet_ip 100.x.0.1   && bad "garbage must NOT be tailnet" || ok "garbage not tailnet"

# --- dks_record / dks_summary ----------------------------------------------
reset
line="$(dks_record demo PASS all good)"
is "record emits one CHECK line" "$line" "CHECK demo PASS all good"
# Tally must be asserted outside a command substitution — $( ) forks a subshell,
# so the counter mutation inside it would not reach us.
reset
dks_record demo PASS all good >/dev/null
is "record tallies pass" "$DKS_PASS_COUNT" "1"

reset
line="$(dks_record demo FAIL "line one
line two")"
is "record collapses newlines to one line" "$line" "CHECK demo FAIL line one | line two"
is "one line only" "$(printf '%s' "$line" | wc -l | tr -d ' ')" "0"

# THE critical invariant: a skipped/unrunnable check is BLOCKED, never PASS.
reset
dks_record skipme BLOCKED "precondition absent" >/dev/null
is "blocked does not count as pass" "$DKS_PASS_COUNT" "0"
is "blocked counts as blocked" "$DKS_BLOCKED_COUNT" "1"
is "blocked does not count as fail" "$DKS_FAIL_COUNT" "0"
case "${DKS_RESULTS[0]}" in
    *" PASS "*) bad "BLOCKED result must never contain PASS" "${DKS_RESULTS[0]}" ;;
    "CHECK skipme BLOCKED precondition absent") ok "BLOCKED recorded verbatim, never PASS" ;;
    *) bad "blocked line shape" "${DKS_RESULTS[0]}" ;;
esac

# An unknown status must degrade to FAIL, not be treated as success.
reset
dks_record weird MAYBE something >/dev/null
is "unknown status degrades to fail" "$DKS_FAIL_COUNT" "1"
is "unknown status is not a pass" "$DKS_PASS_COUNT" "0"

# --- exit-status contract ---------------------------------------------------
reset
dks_record a PASS x >/dev/null; dks_record b BLOCKED y >/dev/null
sum="$(dks_summary)"; rc=$?
is "no failures -> summary rc 0" "$rc" "0"
case "$sum" in *"pass=1 fail=0 blocked=1"*) ok "summary tallies" ;; *) bad "summary tallies" "$sum" ;; esac
case "$sum" in *"RESULT OK"*) ok "summary says OK" ;; *) bad "summary says OK" "$sum" ;; esac

reset
dks_record a PASS x >/dev/null; dks_record b FAIL y >/dev/null
sum="$(dks_summary)"; rc=$?
is "any FAIL -> summary rc 1" "$rc" "1"
case "$sum" in *"RESULT FAIL"*) ok "summary says FAIL" ;; *) bad "summary says FAIL" "$sum" ;; esac

# All-blocked must NOT read as success.
reset
dks_record a BLOCKED x >/dev/null; dks_record b BLOCKED y >/dev/null
sum="$(dks_summary)"; rc=$?
is "all-blocked -> rc 0 but not OK" "$rc" "0"
case "$sum" in *"INCONCLUSIVE"*) ok "all-blocked reports INCONCLUSIVE, not OK" ;; *) bad "all-blocked wording" "$sum" ;; esac

# --- dks_selected -----------------------------------------------------------
unset DKS_ONLY
dks_selected anything && ok "no DKS_ONLY -> all selected" || bad "no DKS_ONLY -> all selected"
DKS_ONLY="nodes-ready,cluster-dns"
dks_selected nodes-ready  && ok "DKS_ONLY selects listed"     || bad "DKS_ONLY selects listed"
dks_selected cluster-dns  && ok "DKS_ONLY selects last item"  || bad "DKS_ONLY selects last item"
dks_selected headlamp     && bad "DKS_ONLY must exclude unlisted" || ok "DKS_ONLY excludes unlisted"
dks_selected nodes        && bad "DKS_ONLY must not prefix-match"  || ok "DKS_ONLY exact-matches only"
unset DKS_ONLY

# --- harness hygiene --------------------------------------------------------
if bash -n "$HARNESS"; then ok "harness parses (bash -n)"; else bad "harness parses (bash -n)"; fi
if grep -nE '(token|password|secret|authkey)[[:space:]]*=[[:space:]]*["'"'"'][A-Za-z0-9]{8,}' "$HARNESS" >/dev/null; then
    bad "harness contains no embedded secret literal"
else
    ok "harness contains no embedded secret literal"
fi
# Every check name in the story must be implemented.
for n in nodes-ready distinct-pod-cidrs flannel-iface no-stale-conflist \
         cross-node-pod-ip service-clusterip cluster-dns logs-exec \
         headlamp nanochat bashy-chunked; do
    if grep -q "dks_selected $n" "$HARNESS"; then ok "check implemented: $n"; else bad "check missing: $n"; fi
done

echo
echo "TESTS pass=$T_PASS fail=$T_FAIL"
[ "$T_FAIL" -eq 0 ]
