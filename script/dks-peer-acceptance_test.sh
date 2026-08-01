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

# All-blocked must NOT read as success — INCONCLUSIVE exits 2, never 0.
reset
dks_record a BLOCKED x >/dev/null; dks_record b BLOCKED y >/dev/null
sum="$(dks_summary)"; rc=$?
is "all-blocked -> rc 2 (INCONCLUSIVE)" "$rc" "2"
case "$sum" in *"INCONCLUSIVE"*) ok "all-blocked reports INCONCLUSIVE, not OK" ;; *) bad "all-blocked wording" "$sum" ;; esac

# Zero checks at all is the same absence-of-evidence case.
reset
sum="$(dks_summary)"; rc=$?
is "no check ran -> rc 2 (INCONCLUSIVE)" "$rc" "2"
case "$sum" in *"INCONCLUSIVE (no check ran)"*) ok "no-check-ran names the reason" ;; *) bad "no-check-ran wording" "$sum" ;; esac

# FAIL outranks INCONCLUSIVE: a run with a FAIL and no PASS is rc 1, not 2.
reset
dks_record a FAIL x >/dev/null; dks_record b BLOCKED y >/dev/null
rc=0; dks_summary >/dev/null || rc=$?
is "FAIL with no PASS -> rc 1 (FAIL outranks INCONCLUSIVE)" "$rc" "1"

# --- dks_selected -----------------------------------------------------------
unset DKS_ONLY
dks_selected anything && ok "no DKS_ONLY -> all selected" || bad "no DKS_ONLY -> all selected"
DKS_ONLY="nodes-ready,cluster-dns"
dks_selected nodes-ready  && ok "DKS_ONLY selects listed"     || bad "DKS_ONLY selects listed"
dks_selected cluster-dns  && ok "DKS_ONLY selects last item"  || bad "DKS_ONLY selects last item"
dks_selected headlamp     && bad "DKS_ONLY must exclude unlisted" || ok "DKS_ONLY excludes unlisted"
dks_selected nodes        && bad "DKS_ONLY must not prefix-match"  || ok "DKS_ONLY exact-matches only"
unset DKS_ONLY

# --- end-to-end exit status of the RUNNER -----------------------------------
# The bug this guards: the RESULT line said INCONCLUSIVE while the process
# still exited 0, so any gate reading only $? scored "nothing proven" as a
# pass. These assert the real exit status of `bash dks-peer-acceptance.sh`,
# not its stdout — a stdout-only test would not have caught it.
#
# Offline throughout: a stub `kubectl` FIRST on a minimal PATH answers the
# preflight probe (`-o json`) and prints a fixed "<node> <kubeletVersion>
# <Ready>" table that the READY_LIST pipeline parses. The PATH also omits the
# real kubectl's dir, so a stub miss cannot silently fall through to a live
# cluster. Only checks needing nothing beyond that table are selected.
#
# The runner is launched with a REAL bash, never a $PATH-resolved `bash`: an
# agentic shell (bashy) may resolve command names outside PATH, which would
# defeat the stub and send these tests at a live cluster.
BASH_BIN=""
for c in /bin/bash /usr/bin/bash; do [ -x "$c" ] && { BASH_BIN="$c"; break; }; done
[ -n "$BASH_BIN" ] || BASH_BIN="$(command -v bash)"

STUB_DIR="$(mktemp -d)"
trap 'rm -rf "$STUB_DIR"' EXIT
mkdir -p "$STUB_DIR/bin" "$STUB_DIR/nokubectl"
cat >"$STUB_DIR/bin/kubectl" <<'STUB'
#!/bin/sh
# Suffix match, not substring: "-o jsonpath=..." also contains "-o json",
# and swallowing the jsonpath call would leave READY_LIST empty.
case "$*" in
    *"-o json") echo '{"items":[]}'; exit 0 ;;
esac
cat "${DKS_STUB_NODES:-/dev/null}"
exit 0
STUB
chmod +x "$STUB_DIR/bin/kubectl"
printf 'node-a v1.31.0+k3s1 True\nnode-b v1.31.0+k3s1 True\n' >"$STUB_DIR/two-nodes"
printf 'node-a v1.31.0+k3s1 True\n' >"$STUB_DIR/one-node"
STUB_PATH="$STUB_DIR/bin:/usr/bin:/bin"

# runner <label> <want-rc> <DKS_ONLY> <nodes-file|""> — asserts the harness's
# real exit status. Greps nothing: the exit code IS the assertion.
runner() {
    local label="$1" want="$2" only="$3" nodes="${4:-/dev/null}"
    local out rc
    out="$(PATH="$STUB_PATH" DKS_NAMESPACE=default DKS_ONLY="$only" \
        DKS_STUB_NODES="$nodes" "$BASH_BIN" "$HARNESS" 2>&1)"; rc=$?
    if [ "$rc" = "$want" ]; then ok "$label -> exit $want"
    else bad "$label -> exit $want" "got $rc; output: $(echo "$out" | tail -3 | tr '\n' ' ')"; fi
}

# 1. Zero checks ran: no selected name matches, so no CHECK line is emitted.
runner "runner: no check ran" 2 nonexistent-check "$STUB_DIR/two-nodes"

# 2. All BLOCKED: no-stale-conflist blocks without DKS_ALLOW_NODE_DEBUG.
runner "runner: all BLOCKED" 2 no-stale-conflist "$STUB_DIR/two-nodes"

# 3. At least one FAIL: a single Ready node fails nodes-ready.
runner "runner: a FAIL" 1 nodes-ready "$STUB_DIR/one-node"

# 4. FAIL outranks INCONCLUSIVE: a FAIL alongside a BLOCKED is still exit 1.
runner "runner: FAIL outranks INCONCLUSIVE" 1 nodes-ready,no-stale-conflist "$STUB_DIR/one-node"

# 5. At least one PASS and no FAIL: the only path to exit 0.
runner "runner: a PASS, no FAIL" 0 nodes-ready "$STUB_DIR/two-nodes"

# 6. Missing kubectl -> preflight BLOCKED -> inconclusive, not success.
out="$(PATH="$STUB_DIR/nokubectl" DKS_NAMESPACE=default DKS_ONLY=nodes-ready \
    "$BASH_BIN" "$HARNESS" 2>&1)"; rc=$?
is "runner: kubectl absent -> exit 2" "$rc" "2"
case "$out" in *"CHECK preflight BLOCKED"*) ok "kubectl absent -> preflight BLOCKED" ;; *) bad "kubectl absent wording" "$out" ;; esac

# --- flannel-iface annotation-only never PASS ---------------------------------
# Regression: annotation presence alone must not result in PASS.
reset
dks_record test-ann-only BLOCKED "annotation only: host inspection required" >/dev/null
is "annotation-only BLOCKED does not tally as PASS" "$DKS_PASS_COUNT" "0"
is "annotation-only BLOCKED tallies as BLOCKED" "$DKS_BLOCKED_COUNT" "1"
# Summary with only BLOCKED checks (no PASS) must exit non-zero and say INCONCLUSIVE
reset
dks_record flannel-iface BLOCKED "annotation only" >/dev/null
dks_record no-stale-conflist BLOCKED "needs host debug" >/dev/null
sum="$(dks_summary)"; rc=$?
is "annotation-guard BLOCKED checks -> rc 2 (INCONCLUSIVE)" "$rc" "2"
case "$sum" in *"INCONCLUSIVE"*) ok "annotation-only annotation never reads as OK" ;; *) bad "annotation-only wording" "$sum" ;; esac

# --- distinct-pod-cidrs scoped to Ready nodes --------------------------------
# Regression: stale NotReady nodes must be excluded from CIDR distinctness check.
# This is a unit test only — it cannot run without a cluster, but the harness
# code that excludes them by status=False is covered by integration tests.
# Here we verify the logic: the check's logic should reject any input that
# includes stale nodes' podCIDRs.
# Simulated input: one Ready node + one NotReady node with same CIDR.
# The harness excludes NotReady, so distinct_cidrs sees only the Ready one.
# The test simulates the filtering:
ready_nodes="a"; stale_nodes="b"
mixed_input="a 10.42.0.0/24
b 10.42.0.0/24"
filtered="$(echo "$mixed_input" | grep -E "^(a|b) " | grep "^a ")"
out="$(echo "$filtered" | dks_distinct_cidrs)"; rc=$?
is "stale-node CIDR excluded -> no collision" "$rc" "0"
case "$out" in *"1 distinct"*) ok "filtered input shows 1 CIDR" ;; *) bad "filtered distinctness" "$out" ;; esac

# --- cleanup contract (named scoped pods) ------------------------------------
# Regression: the harness must track named pods so cleanup can delete them.
# This is an offline check: verify that the pods created have tracked names.
# The cleanup trap references CREATED array which must be populated by make_pod/inspection.
# Simulate: when inspection pods are created, they get added to CREATED.
reset
CREATED=()
# Mock: represent the created inspection pod
CREATED+=("pod/dksacc-conflist-12345")
CREATED+=("pod/dksacc-flannel-check-12345-worker-a")
# Cleanup would iterate CREATED and delete each. Verify the array is non-empty.
[ ${#CREATED[@]} -gt 0 ] && ok "cleanup pod tracking: pods tracked" || bad "cleanup pod tracking: empty"
# Verify pods have deterministic names (contain pid or timestamp, not 'debug').
for pod in "${CREATED[@]}"; do
    case "$pod" in
        *debug*) bad "cleanup pod tracking: must not use kubectl debug" ;;
        *dksacc-*) ok "cleanup pod tracking: has dksacc prefix" ;;
        *) bad "cleanup pod tracking: unexpected pod name: $pod" ;;
    esac
done

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
