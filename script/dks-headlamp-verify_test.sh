#!/usr/bin/env bash
# Offline tests for script/dks-headlamp-verify.sh.
# Needs NO cluster, NO kubectl, NO network. Run: bash script/dks-headlamp-verify_test.sh
#
# Two layers (same shape as script/dks-peer-acceptance_test.sh):
#   1. unit tests of the pure helpers (sourced with HLV_LIB_ONLY=1);
#   2. behavioral tests that execute the REAL runner against a stub kubectl on
#      a minimal PATH plus deterministic probe overrides, asserting exit
#      status and CHECK lines on the running harness itself.
#
# The real /dev/tcp probe path is exercised only in live runs; what IS proven
# here is every scoring decision built on top of it — in particular the
# fail-closed invariant that an unvalidated refusal can never become a PASS.
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
HARNESS="$HERE/dks-headlamp-verify.sh"

T_PASS=0; T_FAIL=0
ok()   { T_PASS=$((T_PASS+1)); echo "ok   - $1"; }
bad()  { T_FAIL=$((T_FAIL+1)); echo "FAIL - $1"; [ -z "${2:-}" ] || echo "       $2"; return 0; }
is()   { if [ "$2" = "$3" ]; then ok "$1"; else bad "$1" "want [$3] got [$2]"; fi; }

FIX="$(mktemp -d)"
trap 'rm -rf "$FIX"' EXIT

# Load the pure helpers only.
HLV_LIB_ONLY=1 . "$HARNESS"

reset() { HLV_PASS_COUNT=0; HLV_FAIL_COUNT=0; HLV_BLOCKED_COUNT=0; HLV_RESULTS=(); }

# --- hlv_record / hlv_summary ------------------------------------------------
reset
line="$(hlv_record demo PASS all good)"
is "record emits one CHECK line" "$line" "CHECK demo PASS all good"
reset
hlv_record demo PASS all good >/dev/null
is "record tallies pass" "$HLV_PASS_COUNT" "1"

reset
line="$(hlv_record demo FAIL "line one
line two")"
is "record collapses newlines to one line" "$line" "CHECK demo FAIL line one | line two"

# THE invariant shared with the acceptance harness: BLOCKED is never a pass.
reset
hlv_record skipme BLOCKED "precondition absent" >/dev/null
is "blocked does not count as pass" "$HLV_PASS_COUNT" "0"
is "blocked counts as blocked" "$HLV_BLOCKED_COUNT" "1"

reset
hlv_record weird MAYBE something >/dev/null
is "unknown status degrades to fail" "$HLV_FAIL_COUNT" "1"
is "unknown status is not a pass" "$HLV_PASS_COUNT" "0"

# THE invariant: a BLOCKED check prevents RESULT OK even when another check PASSes.
# A single passing check cannot carry the verdict after a BLOCKED check —
# a BLOCKED security assurance is worse than no check at all.
reset
hlv_record a PASS x >/dev/null; hlv_record b BLOCKED y >/dev/null
sum="$(hlv_summary)"; rc=$?
is "PASS + BLOCKED -> rc 2, never 0" "$rc" "2"
case "$sum" in *"blocked security assurance is not OK"*) ok "BLOCKED reason states the invariant" ;; *) bad "BLOCKED reason states the invariant" "$sum" ;; esac

reset
hlv_record a PASS x >/dev/null
sum="$(hlv_summary)"; rc=$?
is "all PASS -> rc 0" "$rc" "0"
case "$sum" in *"RESULT OK"*) ok "all PASS says OK" ;; *) bad "all PASS says OK" "$sum" ;; esac

reset
hlv_record a PASS x >/dev/null; hlv_record b FAIL y >/dev/null
sum="$(hlv_summary)"; rc=$?
is "any FAIL -> summary rc 1" "$rc" "1"

reset
hlv_record a BLOCKED x >/dev/null
sum="$(hlv_summary)"; rc=$?
is "all-blocked -> rc 2 (INCONCLUSIVE)" "$rc" "2"
case "$sum" in *INCONCLUSIVE*) ok "all-blocked reports INCONCLUSIVE" ;; *) bad "all-blocked wording" "$sum" ;; esac

reset
sum="$(hlv_summary)"; rc=$?
is "no check ran -> rc 2" "$rc" "2"

reset
hlv_record a FAIL x >/dev/null; hlv_record b BLOCKED y >/dev/null
rc=0; hlv_summary >/dev/null || rc=$?
is "FAIL outranks INCONCLUSIVE" "$rc" "1"

# --- hlv_selected ------------------------------------------------------------
unset HLV_ONLY
hlv_selected anything && ok "no HLV_ONLY -> all selected" || bad "no HLV_ONLY -> all selected"
HLV_ONLY="running,answering"
hlv_selected running   && ok "HLV_ONLY selects listed"    || bad "HLV_ONLY selects listed"
hlv_selected answering && ok "HLV_ONLY selects last item" || bad "HLV_ONLY selects last item"
hlv_selected rbac-zero-grant && bad "HLV_ONLY must exclude unlisted" || ok "HLV_ONLY excludes unlisted"
hlv_selected run       && bad "HLV_ONLY must not prefix-match" || ok "HLV_ONLY exact-matches only"
unset HLV_ONLY

# --- hlv_override_lookup -----------------------------------------------------
is "override lookup hit" "$(hlv_override_lookup "a:1=open b:2=closed" "b:2")" "closed"
is "override lookup first key" "$(hlv_override_lookup "a:1=open b:2=closed" "a:1")" "open"
hlv_override_lookup "a:1=open" "c:3" >/dev/null && bad "override miss must rc 1" || ok "override miss -> rc 1"

# --- hlv_tcp_probe (override path + deterministic error path) ----------------
HLV_PROBE_OVERRIDE="192.0.2.10:4466=closed 192.0.2.10:10250=open"
is "probe override closed" "$(hlv_tcp_probe 192.0.2.10 4466)" "closed"
is "probe override open" "$(hlv_tcp_probe 192.0.2.10 10250)" "open"
is "probe override MISS is error (fail closed), never closed" "$(hlv_tcp_probe 192.0.2.99 4466)" "error"
unset HLV_PROBE_OVERRIDE
# Real path, deterministic failure: an empty host can never connect.
is "probe real path: empty host -> error" "$(hlv_tcp_probe "" 80 1)" "error"

# --- hlv_http_code (override path) -------------------------------------------
HLV_HTTP_OVERRIDE="127.0.0.1:18466=200"
is "http override code" "$(hlv_http_code 127.0.0.1 18466)" "200"
HLV_HTTP_OVERRIDE="127.0.0.1:18466=none"
hlv_http_code 127.0.0.1 18466 >/dev/null && bad "http override none must rc 1" || ok "http override none -> rc 1 (no answer)"
unset HLV_HTTP_OVERRIDE

# --- hlv_score_exposure: the fail-closed core --------------------------------
# PASS only on: target refused AND control proved the host live.
out="$(hlv_score_exposure closed open 10250)"; rc=$?
is "refused + control open -> PASS" "$rc" "0"
case "$out" in *"proven live"*) ok "PASS reason names the control proof" ;; *) bad "PASS reason names the control proof" "$out" ;; esac

# Observed exposure is FAIL regardless of the control's state.
out="$(hlv_score_exposure open open 10250)"; rc=$?
is "open target -> FAIL" "$rc" "1"
out="$(hlv_score_exposure open closed 10250)"; rc=$?
is "open target with dead control still FAIL (exposure observed)" "$rc" "1"

# THE false-green scenario this run must never produce: connection refused for
# the wrong reason (wrong/dead host — control also refused) is BLOCKED.
out="$(hlv_score_exposure closed closed 10250)"; rc=$?
is "refused on unproven host -> BLOCKED, never PASS" "$rc" "2"
case "$out" in *"not evidence of non-exposure"*) ok "BLOCKED reason states the invariant" ;; *) bad "BLOCKED reason states the invariant" "$out" ;; esac

out="$(hlv_score_exposure closed timeout 10250)"; rc=$?
is "refused but control timed out -> BLOCKED" "$rc" "2"
out="$(hlv_score_exposure closed error 10250)"; rc=$?
is "refused but control errored -> BLOCKED" "$rc" "2"

# "Could not determine" is never a pass, even on a proven-live host.
out="$(hlv_score_exposure timeout open 10250)"; rc=$?
is "timeout on live host -> BLOCKED" "$rc" "2"
out="$(hlv_score_exposure error open 10250)"; rc=$?
is "probe error on live host -> BLOCKED" "$rc" "2"

# =============================================================================
# Behavioral tests: the REAL runner against a stub kubectl.
# =============================================================================
# The stub is FIRST on a minimal PATH that omits the real kubectl's dir, so a
# stub miss cannot silently fall through to a live cluster. Probe/HTTP results
# come from the documented offline-test overrides — no network is touched.
BASH_BIN=""
for c in /bin/bash /usr/bin/bash; do [ -x "$c" ] && { BASH_BIN="$c"; break; }; done
[ -n "$BASH_BIN" ] || BASH_BIN="$(command -v bash)"

mkdir -p "$FIX/bin" "$FIX/nokubectl"
# Create a dummy peer kubeconfig so the preflight passes in-test.
PEER_KUBE="$FIX/kubeconfig"
: >"$PEER_KUBE"
cat >"$FIX/bin/kubectl" <<'STUB'
#!/bin/sh
# Discard --kubeconfig <path> args (the harness pins the peer kubeconfig);
# keep the rest for dispatch.
shifty() {
    local sep=""
    while [ $# -gt 0 ]; do
        case "$1" in
            --kubeconfig) shift 2 ;;
            *) printf '%s%s' "$sep" "$1"; sep=" "; shift ;;
        esac
    done
}
ARGS="$(shifty "$@")"
[ -n "${HLV_STUB_LOG:-}" ] && printf '%s\n' "$*" >>"$HLV_STUB_LOG"
case "$ARGS" in
    "get nodes -o name")
        if [ "${HLV_STUB_PREFLIGHT_RC:-0}" != "0" ]; then
            echo "The connection to the server was refused" >&2; exit 1
        fi
        echo "node/control-plane-host"; exit 0 ;;
    *readyReplicas*)
        if [ "${HLV_STUB_DEP:-present}" = "absent" ]; then
            echo 'Error from server (NotFound): deployments.apps "headlamp" not found' >&2; exit 1
        fi
        # No colon: set-but-empty must pass through as empty (readyReplicas
        # is absent from the API object when zero replicas are ready).
        printf '%s' "${HLV_STUB_READY-1}"; exit 0 ;;
    *"{.spec.type}"*)
        case "${HLV_STUB_SVC:-ClusterIP||}" in
            absent) echo 'Error from server (NotFound): services "headlamp" not found' >&2; exit 1 ;;
            error)  echo 'apiserver flake' >&2; exit 1 ;;
            *)      printf '%s' "${HLV_STUB_SVC:-ClusterIP||}"; exit 0 ;;
        esac ;;
    *"get clusterrolebindings"*) cat "${HLV_STUB_CRB:-/dev/null}"; exit 0 ;;
    *"get rolebindings -A"*)     cat "${HLV_STUB_RB:-/dev/null}"; exit 0 ;;
    *InternalIP*) printf '%s' "${HLV_STUB_ADDRS:-}"; exit 0 ;;
    *port-forward*)
        # Long sleep: a forward the runner fails to kill stays visible to
        # the process-liveness leak check after the run.
        sleep 60; exit 0 ;;
esac
exit 0
STUB
chmod +x "$FIX/bin/kubectl"
STUB_PATH="$FIX/bin:/usr/bin:/bin"

# RBAC fixtures: "<binding> <Kind>/<ns>/<name>..." lines (jsonpath shape).
printf 'outpost-headlamp-viewer-view ServiceAccount/headlamp/headlamp-viewer\nsome-other-binding ServiceAccount/kube-system/somebody\n' >"$FIX/crb-clean"
printf 'sneaky-admin ServiceAccount/headlamp/headlamp\n' >"$FIX/crb-grant"
printf 'kube-system/ns-grant ServiceAccount/headlamp/headlamp\n' >"$FIX/rb-grant"
: >"$FIX/rb-empty"

stub_reset() {
    STUB_PREFLIGHT_RC=0; STUB_DEP=present; STUB_READY=1
    STUB_SVC="ClusterIP||"
    STUB_CRB="$FIX/crb-clean"; STUB_RB="$FIX/rb-empty"
    STUB_ADDRS="192.0.2.10 "
    STUB_PROBE="127.0.0.1:18466=open 192.0.2.10:10250=open 192.0.2.10:4466=closed"
    STUB_HTTP="127.0.0.1:18466=200"
    STUB_LOG=""; STUB_ONLY=""; STUB_PROBE_ADDRS=""
}

# run_case — executes the real runner; sets RH_OUT / RH_RC.
run_case() {
    RH_OUT="$(PATH="$STUB_PATH" \
        HLV_ONLY="$STUB_ONLY" HLV_TIMEOUT=3 \
        HLV_STUB_PREFLIGHT_RC="$STUB_PREFLIGHT_RC" HLV_STUB_DEP="$STUB_DEP" \
        HLV_STUB_READY="$STUB_READY" HLV_STUB_SVC="$STUB_SVC" \
        HLV_STUB_CRB="$STUB_CRB" HLV_STUB_RB="$STUB_RB" \
        HLV_STUB_ADDRS="$STUB_ADDRS" HLV_STUB_LOG="$STUB_LOG" \
        HLV_PROBE_OVERRIDE="$STUB_PROBE" HLV_HTTP_OVERRIDE="$STUB_HTTP" \
        HLV_PROBE_ADDRS="$STUB_PROBE_ADDRS" \
        HLV_PEER_KUBECONFIG="$PEER_KUBE" \
        "$BASH_BIN" "$HARNESS" 2>&1)"
    RH_RC=$?
}

# expect <label> <want-rc> [must-contain] [must-not-contain]
expect() {
    local label="$1" want="$2" needle="${3:-}" anti="${4:-}"
    if [ "$RH_RC" = "$want" ]; then ok "$label -> exit $want"
    else bad "$label -> exit $want" "got $RH_RC; $(echo "$RH_OUT" | grep -E 'CHECK|RESULT' | tr '\n' ' ')"; fi
    if [ -n "$needle" ]; then
        case "$RH_OUT" in *"$needle"*) ok "$label -> emits [$needle]" ;;
        *) bad "$label -> emits [$needle]" "$(echo "$RH_OUT" | grep -E 'CHECK|RESULT' | tr '\n' ' ')" ;; esac
    fi
    if [ -n "$anti" ]; then
        case "$RH_OUT" in *"$anti"*) bad "$label -> must NOT emit [$anti]" "$(echo "$RH_OUT" | grep CHECK | tr '\n' ' ')" ;;
        *) ok "$label -> does not emit [$anti]" ;; esac
    fi
}

# --- preflight ---------------------------------------------------------------
out="$(HLV_PEER_KUBECONFIG="$PEER_KUBE" PATH="$FIX/nokubectl:/usr/bin:/bin" "$BASH_BIN" "$HARNESS" 2>&1)"; rc=$?
is "runner: kubectl absent -> exit 2" "$rc" "2"
case "$out" in *"CHECK preflight BLOCKED"*) ok "kubectl absent -> preflight BLOCKED" ;; *) bad "kubectl absent wording" "$out" ;; esac

stub_reset; STUB_PREFLIGHT_RC=1; run_case
expect "runner: cluster unreachable" 2 "CHECK preflight BLOCKED"

# --- the all-green run -------------------------------------------------------
stub_reset; STUB_LOG="$FIX/log-green"; run_case
expect "runner: clean deployment" 0 "RESULT OK"
for want in "CHECK running PASS" "CHECK service-clusterip-only PASS" \
            "CHECK rbac-zero-grant PASS" "CHECK answering PASS" \
            "CHECK no-unauthenticated-exposure PASS"; do
    case "$RH_OUT" in *"$want"*) ok "green run: $want" ;; *) bad "green run: $want" "$RH_OUT" ;; esac
done
# Overrides were active — the run must say so, so a log reader can never
# mistake an offline rehearsal for a real network verification.
case "$RH_OUT" in *"offline test mode"*) ok "green run: override NOTE printed" ;; *) bad "green run: override NOTE printed" "$RH_OUT" ;; esac
# The deterministic-kubeconfig path must be printed.
case "$RH_OUT" in *"kubeconfig="*) ok "green run: kubeconfig NOTE printed" ;; *) bad "green run: kubeconfig NOTE printed" "$RH_OUT" ;; esac
# The port-forward the runner started must be gone (killed on cleanup).
# The stub's forward sleeps 60 s, so anything the runner failed to kill is
# still alive in the process table now. This is checked by process liveness,
# not a stub-side pid log: the runner kills the forward the moment its probe
# passes, and the stub can lose that race before it ever writes a log line.
if pgrep -f "$FIX/bin/kubectl" >/dev/null 2>&1; then
    pkill -f "$FIX/bin/kubectl" 2>/dev/null
    bad "port-forward killed on cleanup" "a stub kubectl outlived the runner"
else
    ok "port-forward killed on cleanup"
fi
# The forward must be pinned to loopback. Asserted against the source line
# the runner executes — the stub's argv log is unreliable for the same
# kill-race reason as above.
if grep -q -- 'port-forward --address 127.0.0.1' "$HARNESS"; then
    ok "port-forward pinned to 127.0.0.1"
else
    bad "port-forward pinned to 127.0.0.1"
fi

# --- running -----------------------------------------------------------------
stub_reset; STUB_DEP=absent; STUB_ONLY=running; run_case
expect "running: deployment absent" 1 "CHECK running FAIL"

stub_reset; STUB_READY=""; STUB_ONLY=running; run_case
expect "running: zero ready replicas" 1 "CHECK running FAIL"

stub_reset; STUB_DEP=absent; run_case
expect "running: absent deployment blocks answering (never false-FAIL)" 1 "CHECK answering BLOCKED" "CHECK answering FAIL"

# --- service-clusterip-only --------------------------------------------------
stub_reset; STUB_SVC="NodePort|30080|"; STUB_ONLY=service-clusterip-only; run_case
expect "shape: NodePort drift" 1 "CHECK service-clusterip-only FAIL"
case "$RH_OUT" in *"nodePorts=[30080]"*) ok "shape: names the nodePort" ;; *) bad "shape: names the nodePort" "$RH_OUT" ;; esac

stub_reset; STUB_SVC="ClusterIP||203.0.113.7"; STUB_ONLY=service-clusterip-only; run_case
expect "shape: externalIPs drift" 1 "CHECK service-clusterip-only FAIL"

stub_reset; STUB_SVC=absent; STUB_ONLY=service-clusterip-only; run_case
expect "shape: service absent" 1 "CHECK service-clusterip-only FAIL"

stub_reset; STUB_SVC=error; STUB_ONLY=service-clusterip-only; run_case
expect "shape: unreadable service -> BLOCKED, never PASS" 2 "CHECK service-clusterip-only BLOCKED" "CHECK service-clusterip-only PASS"

# --- rbac-zero-grant ---------------------------------------------------------
stub_reset; STUB_CRB="$FIX/crb-grant"; STUB_ONLY=rbac-zero-grant; run_case
expect "rbac: clusterrolebinding grant on pod SA" 1 "CHECK rbac-zero-grant FAIL"
case "$RH_OUT" in *sneaky-admin*) ok "rbac: names the binding" ;; *) bad "rbac: names the binding" "$RH_OUT" ;; esac

stub_reset; STUB_RB="$FIX/rb-grant"; STUB_ONLY=rbac-zero-grant; run_case
expect "rbac: namespaced grant on pod SA" 1 "CHECK rbac-zero-grant FAIL"

# The viewer SA's own bindings are expected and must NOT trip the pod-SA
# zero-grant check (field-exact match, no prefix collision).
stub_reset; STUB_ONLY=rbac-zero-grant; run_case
expect "rbac: viewer-SA binding does not false-FAIL" 0 "CHECK rbac-zero-grant PASS"

# --- answering ---------------------------------------------------------------
stub_reset; STUB_HTTP="127.0.0.1:18466=none"; STUB_ONLY=running,answering; run_case
expect "answering: TCP open but no HTTP -> FAIL" 1 "CHECK answering FAIL"

# When answering is BLOCKED and running PASSes, the BLOCKED now prevents
# RESULT OK — the verdict must be INCONCLUSIVE (exit 2), never 0.
stub_reset
STUB_PROBE="127.0.0.1:18466=closed 192.0.2.10:10250=open 192.0.2.10:4466=closed"
STUB_ONLY=running,answering; run_case
expect "answering: forward never opens -> BLOCKED" 2 "CHECK answering BLOCKED" "CHECK answering PASS"

# --- no-unauthenticated-exposure: the fail-closed negative -------------------
NE=no-unauthenticated-exposure

# Exposure observed: container port answers on a node address.
stub_reset; STUB_PROBE="192.0.2.10:10250=open 192.0.2.10:4466=open"
STUB_ONLY=$NE; run_case
expect "exposure: open container port" 1 "CHECK $NE FAIL"

# NodePort drift: the drifted port itself is probed and found open.
stub_reset; STUB_SVC="NodePort|30080|"
STUB_PROBE="192.0.2.10:10250=open 192.0.2.10:4466=closed 192.0.2.10:30080=open"
STUB_ONLY=$NE; run_case
expect "exposure: open nodePort" 1 "CHECK $NE FAIL"
case "$RH_OUT" in *"192.0.2.10:30080"*) ok "exposure: names the open addr:port" ;; *) bad "exposure: names the open addr:port" "$RH_OUT" ;; esac

# THE false-green scenario: refused everywhere, but the control port is also
# refused — wrong/dead host. Must be BLOCKED, never PASS.
stub_reset; STUB_PROBE="192.0.2.10:10250=closed 192.0.2.10:4466=closed"
STUB_ONLY=$NE; run_case
expect "exposure: refusal on unproven host -> BLOCKED" 2 "CHECK $NE BLOCKED" "CHECK $NE PASS"

# Timeout (filtered/unroutable) is indeterminate even on a proven-live host.
stub_reset; STUB_PROBE="192.0.2.10:10250=open 192.0.2.10:4466=timeout"
STUB_ONLY=$NE; run_case
expect "exposure: timeout -> BLOCKED, never PASS" 2 "CHECK $NE BLOCKED" "CHECK $NE PASS"

# A crashed/errored probe is indeterminate.
stub_reset; STUB_PROBE="192.0.2.10:10250=open 192.0.2.10:4466=error"
STUB_ONLY=$NE; run_case
expect "exposure: probe error -> BLOCKED" 2 "CHECK $NE BLOCKED" "CHECK $NE PASS"

# An address the override set does not cover resolves to error -> BLOCKED
# (a probe that answers for the wrong address can never score a PASS).
stub_reset; STUB_ADDRS="192.0.2.10 192.0.2.11 "
STUB_ONLY=$NE; run_case
expect "exposure: one unprovable node blocks the claim" 2 "CHECK $NE BLOCKED" "CHECK $NE PASS"

# No addresses at all: nothing was probed, nothing is proven.
stub_reset; STUB_ADDRS=""
STUB_ONLY=$NE; run_case
expect "exposure: no node addresses -> BLOCKED" 2 "CHECK $NE BLOCKED" "CHECK $NE PASS"

# Unreadable Service: the nodePort set is unknown, so the port list is
# incomplete — must not claim non-exposure from a partial probe.
stub_reset; STUB_SVC=error
STUB_ONLY=$NE; run_case
expect "exposure: unenumerable service ports -> BLOCKED" 2 "CHECK $NE BLOCKED" "CHECK $NE PASS"

# Operator-supplied probe addresses take precedence over the node list.
stub_reset; STUB_PROBE_ADDRS="192.0.2.20"
STUB_PROBE="192.0.2.20:10250=open 192.0.2.20:4466=closed"
STUB_ONLY=$NE; run_case
expect "exposure: HLV_PROBE_ADDRS honoured" 0 "CHECK $NE PASS"

# --- harness hygiene ---------------------------------------------------------
if bash -n "$HARNESS"; then ok "harness parses (bash -n)"; else bad "harness parses (bash -n)"; fi
if grep -nE '(token|password|secret|authkey)[[:space:]]*=[[:space:]]*["'"'"'][A-Za-z0-9]{8,}' "$HARNESS" >/dev/null; then
    bad "harness contains no embedded secret literal"
else
    ok "harness contains no embedded secret literal"
fi
# Every check name in the contract must be implemented.
for n in running service-clusterip-only rbac-zero-grant answering no-unauthenticated-exposure; do
    if grep -q "hlv_selected $n" "$HARNESS"; then ok "check implemented: $n"; else bad "check missing: $n"; fi
done
# The harness must reference the deterministic peer kubeconfig path, never
# rely on ambient resolution.
if grep -q 'outpost-control-plane/k3s.yaml' "$HARNESS"; then ok "harness pins the peer kubeconfig"; else bad "harness pins the peer kubeconfig"; fi

echo
echo "TESTS pass=$T_PASS fail=$T_FAIL"
[ "$T_FAIL" -eq 0 ]
