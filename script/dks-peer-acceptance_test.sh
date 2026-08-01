#!/usr/bin/env bash
# Offline tests for script/dks-peer-acceptance.sh.
# Needs NO cluster, NO kubectl, NO network. Run: bash script/dks-peer-acceptance_test.sh
#
# Two layers:
#   1. unit tests of the pure helpers (sourced with DKS_LIB_ONLY=1);
#   2. behavioral tests that execute the REAL runner against a stub kubectl
#      on a minimal PATH and assert its exit status, its CHECK lines, and the
#      manifests/deletes the stub actually received. Nothing here simulates
#      the harness's logic by hand — every invariant is observed on the
#      running harness itself.
set -uo pipefail

# $0 rather than BASH_SOURCE: this file is always executed, never sourced, and
# BASH_SOURCE is not populated by every bash-compatible runner (e.g. bashy).
HERE="$(cd "$(dirname "$0")" && pwd)"
HARNESS="$HERE/dks-peer-acceptance.sh"

T_PASS=0; T_FAIL=0
ok()   { T_PASS=$((T_PASS+1)); echo "ok   - $1"; }
bad()  { T_FAIL=$((T_FAIL+1)); echo "FAIL - $1"; [ -z "${2:-}" ] || echo "       $2"; return 0; }
is()   { if [ "$2" = "$3" ]; then ok "$1"; else bad "$1" "want [$3] got [$2]"; fi; }

FIX="$(mktemp -d)"
trap 'rm -rf "$FIX"' EXIT

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

# --- dks_ev / dks_file_ev (the shared host-evidence vocabulary) ---------------
blob="EV k3s_argv=flannel-iface-tailscale0
EV tailscale0=ipv4:100.64.0.9
noise line"
is "dks_ev extracts a value" "$(dks_ev "$blob" tailscale0)" "ipv4:100.64.0.9"
is "dks_ev extracts another key" "$(dks_ev "$blob" k3s_argv)" "flannel-iface-tailscale0"
is "dks_ev missing key -> empty (missing evidence, never FAIL)" "$(dks_ev "$blob" cni_confdir)" ""
is "dks_ev empty blob -> empty" "$(dks_ev "" k3s_argv)" ""

printf 'node-a k3s_argv=flannel-iface-tailscale0 tailscale0=ipv4:100.64.0.5\nnode-b cni_confdir=empty\n' >"$FIX/hostev-unit"
DKS_HOST_EVIDENCE="$FIX/hostev-unit"
is "dks_file_ev extracts per-node value" "$(dks_file_ev node-a tailscale0)" "ipv4:100.64.0.5"
is "dks_file_ev second node, other key" "$(dks_file_ev node-b cni_confdir)" "empty"
is "dks_file_ev unknown node -> empty" "$(dks_file_ev node-x tailscale0)" ""
is "dks_file_ev unknown key -> empty" "$(dks_file_ev node-a cni_confdir)" ""
unset DKS_HOST_EVIDENCE
is "dks_file_ev no file -> empty" "$(dks_file_ev node-a tailscale0 || true)" ""

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

# =============================================================================
# Behavioral tests: the REAL runner against a stub kubectl.
# =============================================================================
# The stub is FIRST on a minimal PATH that omits the real kubectl's dir, so a
# stub miss cannot silently fall through to a live cluster. It answers the
# harness's actual queries from fixture files, records every invocation (and
# every applied manifest) to DKS_STUB_LOG, and never talks to any cluster.
#
# The runner is launched with a REAL bash, never a $PATH-resolved `bash`: an
# agentic shell (bashy) may resolve command names outside PATH, which would
# defeat the stub and send these tests at a live cluster.
BASH_BIN=""
for c in /bin/bash /usr/bin/bash; do [ -x "$c" ] && { BASH_BIN="$c"; break; }; done
[ -n "$BASH_BIN" ] || BASH_BIN="$(command -v bash)"

mkdir -p "$FIX/bin" "$FIX/nokubectl"
cat >"$FIX/bin/kubectl" <<'STUB'
#!/bin/sh
[ -n "${DKS_STUB_LOG:-}" ] && printf '%s\n' "$*" >>"$DKS_STUB_LOG"
case "$*" in
    *" apply -f -")
        # Consume + record the manifest so tests can assert the pod spec the
        # harness actually submitted (hostPID/hostNetwork/hostPath, names).
        if [ -n "${DKS_STUB_LOG:-}" ]; then sed 's/^/APPLY> /' >>"$DKS_STUB_LOG"; else cat >/dev/null; fi
        exit "${DKS_STUB_APPLY_RC:-0}" ;;
    # Suffix match, not substring: "-o jsonpath=..." also contains "-o json",
    # and swallowing a jsonpath call would corrupt a fixture answer.
    *"-o json") echo '{"items":[]}'; exit 0 ;;
    *custom-columns*) cat "${DKS_STUB_CIDRS:-/dev/null}"; exit 0 ;;
    *public-ip*) cat "${DKS_STUB_ANN:-/dev/null}"; exit 0 ;;
    *kubeletVersion*Ready*) cat "${DKS_STUB_NODES:-/dev/null}"; exit 0 ;;
    *"{.status.phase}") echo "${DKS_STUB_POD_PHASE:-Succeeded}"; exit 0 ;;
    *" logs "*)
        case "$*" in
            *"-insp-"*"-0") cat "${DKS_STUB_EV_A:-/dev/null}" ;;
            *"-insp-"*"-1") cat "${DKS_STUB_EV_B:-/dev/null}" ;;
        esac
        exit 0 ;;
    *"rollout status deployment/"*) exit "${DKS_STUB_ROLLOUT_RC:-0}" ;;
    *"app="*".spec.nodeName"*) cat "${DKS_STUB_NC_NODES:-/dev/null}"; exit 0 ;;
    *"app="*"status.phase=Running"*) cat "${DKS_STUB_NC_READY:-/dev/null}"; exit 0 ;;
    *"app="*"waiting.reason"*) cat "${DKS_STUB_NC_PULL:-/dev/null}"; exit 0 ;;
    *"pod "*"-a -o jsonpath={.status.containerStatuses[0].ready}") echo "${DKS_STUB_READY_A:-false}"; exit 0 ;;
    *"pod "*"-b -o jsonpath={.status.containerStatuses[0].ready}") echo "${DKS_STUB_READY_B:-false}"; exit 0 ;;
esac
exit 0
STUB
chmod +x "$FIX/bin/kubectl"
STUB_PATH="$FIX/bin:/usr/bin:/bin"

# Fixtures. READY_LIST table: "<node> <kubeletVersion> <Ready>".
printf 'node-a v1.31.0+k3s1 True\nnode-b v1.31.0+k3s1 True\n' >"$FIX/two-nodes"
printf 'node-a v1.31.0+k3s1 True\n' >"$FIX/one-node"
# Annotation table: "<node> <flannel public-ip>".
printf 'node-a 100.64.0.5\nnode-b 100.64.0.6\n' >"$FIX/ann-good"
printf 'node-a 100.64.0.5\nnode-b\n' >"$FIX/ann-missing-b"
printf 'node-a 100.64.0.5\nnode-b 10.0.0.6\n' >"$FIX/ann-bad-b"
# Inspection-pod EV output per node.
printf 'EV k3s_argv=flannel-iface-tailscale0\nEV tailscale0=ipv4:100.64.0.5\nEV cni_confdir=empty\n' >"$FIX/ev-good"
# Matches node-b's annotation in ann-good (100.64.0.6) -- the second node's
# own evidence, distinct from ev-good's (which is node-a's address).
printf 'EV k3s_argv=flannel-iface-tailscale0\nEV tailscale0=ipv4:100.64.0.6\nEV cni_confdir=empty\n' >"$FIX/ev-good-b"
printf 'EV k3s_argv=absent\nEV tailscale0=tool-missing\n' >"$FIX/ev-missing"
printf 'EV k3s_argv=no-flannel-iface\nEV tailscale0=ipv4:100.64.0.6\n' >"$FIX/ev-contra-argv"
printf 'EV k3s_argv=flannel-iface-tailscale0\nEV tailscale0=absent\nEV cni_confdir=listing:10-flannel.conflist,10-outpost.conflist,\n' >"$FIX/ev-stale"
printf 'EV cni_confdir=unreadable\n' >"$FIX/ev-unreadable"
# Well-formed (tailnet-shaped) but a DIFFERENT address than ann-good's
# node-b annotation (100.64.0.6) -- the false-PASS scenario the equality
# fix closes: both the annotation and tailscale0 look individually fine.
printf 'EV k3s_argv=flannel-iface-tailscale0\nEV tailscale0=ipv4:100.64.9.9\nEV cni_confdir=empty\n' >"$FIX/ev-mismatch-b"
# Operator host-evidence files.
printf 'node-a k3s_argv=flannel-iface-tailscale0 tailscale0=ipv4:100.64.0.5 cni_confdir=empty\nnode-b k3s_argv=flannel-iface-tailscale0 tailscale0=ipv4:100.64.0.6 cni_confdir=empty\n' >"$FIX/hostev-both"
printf 'node-a k3s_argv=flannel-iface-tailscale0 tailscale0=ipv4:100.64.0.5\n' >"$FIX/hostev-one"
# Node table for distinct-pod-cidrs: "NAME READY PODCIDR VER". node-stale is a
# REAL (kubelet-backed) NotReady node holding a CIDR that collides with node-a;
# node-virt is a virtual-kubelet node colliding with node-b.
printf 'node-a True 10.42.0.0/24 v1.31.0+k3s1\nnode-b True 10.42.1.0/24 v1.31.0+k3s1\nnode-stale False 10.42.0.0/24 v1.31.0+k3s1\nnode-virt True 10.42.1.0/24 v0.1.0-vknode\n' >"$FIX/cidrs-excluded-dups"
printf 'node-a True 10.42.0.0/24 v1.31.0+k3s1\nnode-b True 10.42.0.0/24 v1.31.0+k3s1\n' >"$FIX/cidrs-ready-dup"

# nanochat placement/readiness fixtures.
printf 'node-a\nnode-b\n' >"$FIX/nc-nodes-2"     # 2 distinct nodes
printf 'node-a\nnode-a\n' >"$FIX/nc-nodes-1"     # both replicas on one node
printf 'pod/x\npod/y\n' >"$FIX/nc-ready-2"       # 2 Running pods
printf 'pod/x\n' >"$FIX/nc-ready-1"              # 1 Running pod
printf 'ImagePullBackOff\n' >"$FIX/nc-pull-issue"

stub_reset() {
    STUB_NODES="$FIX/two-nodes"; STUB_CIDRS=/dev/null; STUB_ANN=/dev/null
    STUB_EV_A=/dev/null; STUB_EV_B=/dev/null; STUB_LOG=""
    STUB_APPLY_RC=0; STUB_PHASE=Succeeded; STUB_DEBUG=0; STUB_HOSTEV=""
    STUB_TIMEOUT=5
    STUB_ROLLOUT_RC=0; STUB_NC_NODES=/dev/null; STUB_NC_READY=/dev/null
    STUB_NC_PULL=/dev/null; STUB_NANOCHAT_IMG="stub/nanochat:latest"
    STUB_READY_A=false; STUB_READY_B=false
}

# run_case <DKS_ONLY> — executes the real runner; sets RH_OUT / RH_RC.
run_case() {
    RH_OUT="$(PATH="$STUB_PATH" DKS_NAMESPACE=default DKS_ONLY="$1" \
        DKS_STUB_NODES="$STUB_NODES" DKS_STUB_CIDRS="$STUB_CIDRS" \
        DKS_STUB_ANN="$STUB_ANN" DKS_STUB_EV_A="$STUB_EV_A" \
        DKS_STUB_EV_B="$STUB_EV_B" DKS_STUB_LOG="$STUB_LOG" \
        DKS_STUB_APPLY_RC="$STUB_APPLY_RC" DKS_STUB_POD_PHASE="$STUB_PHASE" \
        DKS_ALLOW_NODE_DEBUG="$STUB_DEBUG" DKS_HOST_EVIDENCE="$STUB_HOSTEV" \
        DKS_TIMEOUT="$STUB_TIMEOUT" \
        DKS_STUB_ROLLOUT_RC="$STUB_ROLLOUT_RC" DKS_STUB_NC_NODES="$STUB_NC_NODES" \
        DKS_STUB_NC_READY="$STUB_NC_READY" DKS_STUB_NC_PULL="$STUB_NC_PULL" \
        DKS_NANOCHAT_IMAGE="$STUB_NANOCHAT_IMG" \
        DKS_STUB_READY_A="$STUB_READY_A" DKS_STUB_READY_B="$STUB_READY_B" \
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

# --- exit-code contract of the real runner -----------------------------------
# The bug this guards: the RESULT line said INCONCLUSIVE while the process
# still exited 0, so any gate reading only $? scored "nothing proven" as pass.
stub_reset; run_case nonexistent-check
expect "runner: no check ran" 2

stub_reset; run_case no-stale-conflist
expect "runner: all BLOCKED" 2

stub_reset; STUB_NODES="$FIX/one-node"; run_case nodes-ready
expect "runner: a FAIL" 1

stub_reset; STUB_NODES="$FIX/one-node"; run_case nodes-ready,no-stale-conflist
expect "runner: FAIL outranks INCONCLUSIVE" 1

stub_reset; run_case nodes-ready
expect "runner: a PASS, no FAIL" 0

out="$(PATH="$FIX/nokubectl" DKS_NAMESPACE=default DKS_ONLY=nodes-ready \
    "$BASH_BIN" "$HARNESS" 2>&1)"; rc=$?
is "runner: kubectl absent -> exit 2" "$rc" "2"
case "$out" in *"CHECK preflight BLOCKED"*) ok "kubectl absent -> preflight BLOCKED" ;; *) bad "kubectl absent wording" "$out" ;; esac

# --- flannel-iface: annotation alone can never PASS ---------------------------
stub_reset; STUB_ANN="$FIX/ann-good"
run_case flannel-iface
expect "flannel: annotation-only" 2 "CHECK flannel-iface BLOCKED" "CHECK flannel-iface PASS"

# Inspection permitted but pods return no evidence: still BLOCKED, never PASS/FAIL.
stub_reset; STUB_ANN="$FIX/ann-good"; STUB_DEBUG=1
run_case flannel-iface
expect "flannel: inspection yields no evidence" 2 "CHECK flannel-iface BLOCKED" "CHECK flannel-iface FAIL"

# --- flannel-iface: full host evidence on BOTH nodes is the only PASS ---------
# ev-good-b (not ev-good) for NODE_B: ann-good gives node-a=100.64.0.5 and
# node-b=100.64.0.6, so each node's tailscale0 evidence must match ITS OWN
# annotation, not just be independently well-formed.
stub_reset; STUB_ANN="$FIX/ann-good"; STUB_DEBUG=1
STUB_EV_A="$FIX/ev-good"; STUB_EV_B="$FIX/ev-good-b"; STUB_LOG="$FIX/log-flannel-pass"
run_case flannel-iface
expect "flannel: full evidence both nodes" 0 "CHECK flannel-iface PASS"
case "$RH_OUT" in *"annotation-equals-tailscale0"*) ok "flannel: equality evidence named on PASS" ;; *) bad "flannel: equality evidence named on PASS" "$RH_OUT" ;; esac

# --- flannel-iface: annotation and tailscale0 individually valid but for
# DIFFERENT addresses on the SAME node must FAIL, never PASS -----------------
stub_reset; STUB_ANN="$FIX/ann-good"; STUB_DEBUG=1
STUB_EV_A="$FIX/ev-good"; STUB_EV_B="$FIX/ev-mismatch-b"
run_case flannel-iface
expect "flannel: annotation vs tailscale0 mismatch" 1 "CHECK flannel-iface FAIL" "CHECK flannel-iface PASS"
case "$RH_OUT" in *"annotation-vs-tailscale0-mismatch"*) ok "flannel: mismatch reason named" ;; *) bad "flannel: mismatch reason named" "$RH_OUT" ;; esac

# The applied inspection-pod manifests must request host visibility — an
# ordinary pod cannot see host k3s argv, host tailscale0, or /etc/cni/net.d.
for want in "hostPID: true" "hostNetwork: true" "mountPath: /host, readOnly: true" "hostPath: {path: /," "-insp-flannel-0" "-insp-flannel-1"; do
    if grep -F -- "$want" "$FIX/log-flannel-pass" | grep -q '^APPLY>'; then
        ok "inspection pod manifest has [$want]"
    else
        bad "inspection pod manifest has [$want]" "$(grep '^APPLY>' "$FIX/log-flannel-pass" | head -30)"
    fi
done
# Explicit lifecycle: logs fetched and pod deleted, no `kubectl run --rm`.
grep -qE ' logs .*-insp-flannel-0' "$FIX/log-flannel-pass" && ok "inspection pod logs fetched explicitly" || bad "inspection pod logs fetched explicitly"
grep -qE 'delete pod .*-insp-flannel-0' "$FIX/log-flannel-pass" && ok "inspection pod deleted explicitly" || bad "inspection pod deleted explicitly"

# --- flannel-iface: one-of-two host evidence cannot PASS ----------------------
stub_reset; STUB_ANN="$FIX/ann-good"; STUB_DEBUG=1
STUB_EV_A="$FIX/ev-good"; STUB_EV_B="$FIX/ev-missing"
run_case flannel-iface
expect "flannel: one-of-two evidence" 2 "CHECK flannel-iface BLOCKED" "CHECK flannel-iface PASS"

# Missing tool / invisible process is MISSING evidence -> BLOCKED, never FAIL.
case "$RH_OUT" in *"CHECK flannel-iface FAIL"*) bad "flannel: missing evidence never FAIL" "$RH_OUT" ;; *) ok "flannel: missing evidence never FAIL" ;; esac

# --- flannel-iface: observed contradictions FAIL ------------------------------
stub_reset; STUB_ANN="$FIX/ann-good"; STUB_DEBUG=1
STUB_EV_A="$FIX/ev-good"; STUB_EV_B="$FIX/ev-contra-argv"
run_case flannel-iface
expect "flannel: argv lacks the flag on one node" 1 "CHECK flannel-iface FAIL"

stub_reset; STUB_ANN="$FIX/ann-bad-b"; STUB_DEBUG=1
STUB_EV_A="$FIX/ev-good"; STUB_EV_B="$FIX/ev-good"
run_case flannel-iface
expect "flannel: non-tailnet annotation" 1 "CHECK flannel-iface FAIL"

# --- flannel-iface: BOTH selected annotations required ------------------------
stub_reset; STUB_ANN="$FIX/ann-missing-b"; STUB_DEBUG=1
STUB_EV_A="$FIX/ev-good"; STUB_EV_B="$FIX/ev-good"
run_case flannel-iface
expect "flannel: one annotation missing" 2 "CHECK flannel-iface BLOCKED" "CHECK flannel-iface PASS"

# --- flannel-iface: both selected workers required ----------------------------
stub_reset; STUB_NODES="$FIX/one-node"; STUB_ANN="$FIX/ann-good"; STUB_DEBUG=1
STUB_EV_A="$FIX/ev-good"; STUB_EV_B="$FIX/ev-good"
run_case flannel-iface
expect "flannel: single Ready worker" 2 "CHECK flannel-iface BLOCKED" "CHECK flannel-iface PASS"

# --- flannel-iface: explicit host-evidence input, no inspection RBAC ----------
stub_reset; STUB_ANN="$FIX/ann-good"; STUB_HOSTEV="$FIX/hostev-both"
run_case flannel-iface
expect "flannel: DKS_HOST_EVIDENCE both nodes" 0 "CHECK flannel-iface PASS"

stub_reset; STUB_ANN="$FIX/ann-good"; STUB_HOSTEV="$FIX/hostev-one"
run_case flannel-iface
expect "flannel: DKS_HOST_EVIDENCE one node only" 2 "CHECK flannel-iface BLOCKED" "CHECK flannel-iface PASS"

# --- no-stale-conflist -------------------------------------------------------
stub_reset
run_case no-stale-conflist
expect "conflist: no inspection, no evidence" 2 "CHECK no-stale-conflist BLOCKED"

stub_reset; STUB_DEBUG=1; STUB_EV_A="$FIX/ev-good"; STUB_EV_B="$FIX/ev-good"
run_case no-stale-conflist
expect "conflist: clean on both nodes" 0 "CHECK no-stale-conflist PASS"

stub_reset; STUB_DEBUG=1; STUB_EV_A="$FIX/ev-good"; STUB_EV_B="$FIX/ev-stale"
run_case no-stale-conflist
expect "conflist: stale conflist observed" 1 "CHECK no-stale-conflist FAIL"

stub_reset; STUB_DEBUG=1; STUB_EV_A="$FIX/ev-good"; STUB_EV_B="$FIX/ev-unreadable"
run_case no-stale-conflist
expect "conflist: unreadable stays BLOCKED" 2 "CHECK no-stale-conflist BLOCKED" "CHECK no-stale-conflist PASS"

# --- distinct-pod-cidrs: NotReady + virtual nodes excluded --------------------
# node-stale (NotReady, real) and node-virt (vknode) both collide with a Ready
# node's CIDR; both must be excluded, so the check PASSes on the Ready pair.
stub_reset; STUB_CIDRS="$FIX/cidrs-excluded-dups"
run_case distinct-pod-cidrs
expect "cidrs: NotReady/virtual excluded" 0 "CHECK distinct-pod-cidrs PASS"
case "$RH_OUT" in *"excluded from distinct-pod-cidrs (NotReady): node-stale"*) ok "cidrs: NotReady exclusion named" ;; *) bad "cidrs: NotReady exclusion named" "$RH_OUT" ;; esac
case "$RH_OUT" in *"excluded from distinct-pod-cidrs (virtual-kubelet): node-virt"*) ok "cidrs: virtual exclusion named" ;; *) bad "cidrs: virtual exclusion named" "$RH_OUT" ;; esac

# A duplicate between two READY kubelet-backed nodes must still FAIL.
stub_reset; STUB_CIDRS="$FIX/cidrs-ready-dup"
run_case distinct-pod-cidrs
expect "cidrs: Ready duplicate still fails" 1 "CHECK distinct-pod-cidrs FAIL"

# --- service-clusterip / cluster-dns: BLOCKED when the source probe (POD_A)
# never became Ready, even though the backend (POD_B) and its Service are
# fine. Before this gate, `kubectl exec` into a not-Ready POD_A produced an
# error string that satisfied the FAIL heuristic below it -- a false FAIL for
# a precondition that was never met, not a contradiction of clusterIP routing.
stub_reset; STUB_READY_A=false; STUB_READY_B=true; STUB_TIMEOUT=1
run_case service-clusterip
expect "service-clusterip: source probe not Ready -> BLOCKED" 2 "CHECK service-clusterip BLOCKED" "CHECK service-clusterip FAIL"
case "$RH_OUT" in *"source probe pod on node-a did not become Ready"*) ok "service-clusterip: ERR_A reason named" ;; *) bad "service-clusterip: ERR_A reason named" "$RH_OUT" ;; esac

stub_reset; STUB_READY_A=false; STUB_READY_B=true; STUB_TIMEOUT=1
run_case cluster-dns
expect "cluster-dns: source probe not Ready -> BLOCKED" 2 "CHECK cluster-dns BLOCKED" "CHECK cluster-dns FAIL"
case "$RH_OUT" in *"source probe pod on node-a did not become Ready"*) ok "cluster-dns: ERR_A reason named" ;; *) bad "cluster-dns: ERR_A reason named" "$RH_OUT" ;; esac

# --- nanochat: bounded rollout (DKS_TIMEOUT), not a fixed sleep --------------
stub_reset; STUB_ROLLOUT_RC=0; STUB_NC_NODES="$FIX/nc-nodes-2"; STUB_NC_READY="$FIX/nc-ready-2"
run_case nanochat
expect "nanochat: rollout succeeds, 2 replicas across 2 nodes -> PASS" 0 "CHECK nanochat PASS"

# A slow/failed image pull is a missing precondition, not a placement defect:
# it must BLOCK, never FAIL, and must name the precondition.
stub_reset; STUB_ROLLOUT_RC=1; STUB_NC_PULL="$FIX/nc-pull-issue"
run_case nanochat
expect "nanochat: image never pullable within DKS_TIMEOUT -> BLOCKED" 2 "CHECK nanochat BLOCKED" "CHECK nanochat FAIL"
case "$RH_OUT" in *"not pullable within"*) ok "nanochat: pull-issue reason named" ;; *) bad "nanochat: pull-issue reason named" "$RH_OUT" ;; esac

# A real placement/scheduling defect (rollout never completes, no pull issue
# observed) must still FAIL -- the BLOCKED carve-out is for image pulls only.
stub_reset; STUB_ROLLOUT_RC=1; STUB_NC_NODES="$FIX/nc-nodes-1"; STUB_NC_READY="$FIX/nc-ready-1"
run_case nanochat
expect "nanochat: real placement defect (no pull issue) -> FAIL" 1 "CHECK nanochat FAIL" "CHECK nanochat BLOCKED"

# Pre-existing contract (now exercised): no image configured -> BLOCKED.
stub_reset; STUB_NANOCHAT_IMG=""
run_case nanochat
expect "nanochat: DKS_NANOCHAT_IMAGE unset -> BLOCKED" 2 "CHECK nanochat BLOCKED"

# --- cleanup contract: every inspection pod deleted, even on create/wait fail -
# Create fails: kubectl apply exits 1 for every pod. The pods were registered
# BEFORE creation, so a delete must still be issued for each of them.
stub_reset; STUB_ANN="$FIX/ann-good"; STUB_DEBUG=1
STUB_APPLY_RC=1; STUB_LOG="$FIX/log-create-fail"
run_case flannel-iface
expect "cleanup: create fails" 2 "CHECK flannel-iface BLOCKED"
for p in insp-flannel-0 insp-flannel-1; do
    if grep -qE "delete pod.*$p" "$FIX/log-create-fail"; then
        ok "cleanup: delete issued for $p after create failure"
    else
        bad "cleanup: delete issued for $p after create failure" "$(grep delete "$FIX/log-create-fail")"
    fi
done

# Wait times out: pod never leaves Pending. Delete must still be issued.
stub_reset; STUB_ANN="$FIX/ann-good"; STUB_DEBUG=1
STUB_PHASE=Pending; STUB_TIMEOUT=1; STUB_LOG="$FIX/log-wait-fail"
run_case flannel-iface
expect "cleanup: wait times out" 2 "CHECK flannel-iface BLOCKED"
for p in insp-flannel-0 insp-flannel-1; do
    if grep -qE "delete pod.*$p" "$FIX/log-wait-fail"; then
        ok "cleanup: delete issued for $p after wait timeout"
    else
        bad "cleanup: delete issued for $p after wait timeout" "$(grep delete "$FIX/log-wait-fail")"
    fi
done

# Registration must precede creation in the log: the first apply of an
# inspection pod may never appear before nothing — assert the delete for a pod
# whose apply FAILED (above) proves order; here assert normal runs also delete.
grep -qE 'delete pod.*insp-flannel-1' "$FIX/log-flannel-pass" && ok "cleanup: normal run deletes every inspection pod" || bad "cleanup: normal run deletes every inspection pod"

# --- harness hygiene --------------------------------------------------------
if bash -n "$HARNESS"; then ok "harness parses (bash -n)"; else bad "harness parses (bash -n)"; fi
if grep -nE '(token|password|secret|authkey)[[:space:]]*=[[:space:]]*["'"'"'][A-Za-z0-9]{8,}' "$HARNESS" >/dev/null; then
    bad "harness contains no embedded secret literal"
else
    ok "harness contains no embedded secret literal"
fi
# The retired mechanisms must not come back: `kubectl run --rm` cannot be
# tracked for cleanup, and `kubectl debug node/` leaks unnamed debugger pods.
grep -q 'kubectl run' "$HARNESS" && bad "harness must not use kubectl run" || ok "harness does not use kubectl run"
grep -q 'kubectl debug' "$HARNESS" && bad "harness must not use kubectl debug" || ok "harness does not use kubectl debug"
# Every check name in the story must be implemented.
for n in nodes-ready distinct-pod-cidrs flannel-iface no-stale-conflist \
         cross-node-pod-ip service-clusterip cluster-dns logs-exec \
         headlamp nanochat bashy-chunked; do
    if grep -q "dks_selected $n" "$HARNESS"; then ok "check implemented: $n"; else bad "check missing: $n"; fi
done

echo
echo "TESTS pass=$T_PASS fail=$T_FAIL"
[ "$T_FAIL" -eq 0 ]
