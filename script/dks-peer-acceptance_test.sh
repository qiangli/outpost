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
#
# Sections marked "FALSE-GREEN" reproduce a way the harness could previously
# report success (or a wrong verdict) without the underlying fact being true.
# Each of those tests FAILS against the pre-corrective harness. To verify that
# claim yourself:
#     git show <pre-corrective-sha>:script/dks-peer-acceptance.sh >/tmp/old.sh
#     DKS_HARNESS=/tmp/old.sh bash script/dks-peer-acceptance_test.sh
set -uo pipefail

# $0 rather than BASH_SOURCE: this file is always executed, never sourced, and
# BASH_SOURCE is not populated by every bash-compatible runner (e.g. bashy).
HERE="$(cd "$(dirname "$0")" && pwd)"
# DKS_HARNESS exists so this suite can be pointed at an OLD copy of the runner
# to demonstrate that each FALSE-GREEN test actually fails before the fix.
HARNESS="${DKS_HARNESS:-$HERE/dks-peer-acceptance.sh}"

T_PASS=0; T_FAIL=0
ok()   { T_PASS=$((T_PASS+1)); echo "ok   - $1"; }
bad()  { T_FAIL=$((T_FAIL+1)); echo "FAIL - $1"; [ -z "${2:-}" ] || echo "       $2"; return 0; }
is()   { if [ "$2" = "$3" ]; then ok "$1"; else bad "$1" "want [$3] got [$2]"; fi; }

FIX="$(mktemp -d)"
trap 'rm -rf "$FIX"' EXIT

# Load the pure helpers only.
DKS_LIB_ONLY=1 . "$HARNESS"

# --- legacy-harness tolerance -------------------------------------------------
# This suite must be runnable against a PRE-CORRECTIVE harness (DKS_HARNESS=...)
# so that each FALSE-GREEN test can be OBSERVED to fail before the fix. A
# pre-corrective harness has no `dks_is_precondition` / `dks_resolve_ev` and
# none of the counters the corrective added; without these stand-ins the suite
# would abort under `set -u` at the first reference ("unbound variable") and
# would then prove nothing about any test after it — the documented
# verification command would crash rather than demonstrate anything.
#
# Every stand-in deliberately returns the answer a harness LACKING that
# machinery deserves, so the assertion resting on it FAILs and the run
# continues. The shim never runs against the current harness (each guard is
# skipped when the real definition is present), so it cannot mask a regression.
: "${DKS_ATTESTED_COUNT:=0}"
: "${DKS_SUBSTANTIVE_PASS_COUNT:=0}"
: "${DKS_VENUE_STATUS:=}"
: "${DKS_EV_VALUE:=}"
: "${DKS_EV_SOURCE:=}"
HARNESS_LEGACY=0
if ! command -v dks_is_precondition >/dev/null 2>&1; then
    HARNESS_LEGACY=1
    # A harness with no precondition/substantive distinction classifies nothing
    # as a precondition — which is precisely FALSE-GREEN #1.
    dks_is_precondition() { return 1; }
fi
if ! command -v dks_is_evidence >/dev/null 2>&1; then
    HARNESS_LEGACY=1
    # A harness with no evidence ALLOWLIST decided what counts as proof by
    # blacklist — "anything that is not a precondition" — which is exactly
    # FALSE-GREEN #5: an API-only check counts as evidence by default.
    dks_is_evidence() { ! dks_is_precondition "${1:-}"; }
fi
if ! command -v dks_resolve_ev >/dev/null 2>&1; then
    HARNESS_LEGACY=1
    # A harness that does not track provenance reports none.
    dks_resolve_ev() { DKS_EV_VALUE=""; DKS_EV_SOURCE="harness-lacks-dks_resolve_ev"; return 1; }
fi
if [ "$HARNESS_LEGACY" = "1" ]; then
    echo "NOTE harness [$HARNESS] predates the sprint-42 corrective (missing"
    echo "NOTE dks_is_precondition and/or dks_resolve_ev). The FALSE-GREEN tests"
    echo "NOTE are EXPECTED to fail against it; that is what they exist to show."
fi

reset() {
    DKS_PASS_COUNT=0; DKS_FAIL_COUNT=0; DKS_BLOCKED_COUNT=0
    DKS_ATTESTED_COUNT=0; DKS_SUBSTANTIVE_PASS_COUNT=0
    DKS_VENUE_STATUS=""
    DKS_RESULTS=()
}

# Most summary-level unit tests are about something OTHER than the venue rule,
# and every one of them would now be capped at INCONCLUSIVE by an unasserted
# venue. venue_ok() records the venue PASS those cases assume, so each test
# still exercises the invariant it was written for. The venue rule itself is
# asserted separately, below.
venue_ok() { dks_record peer-venue PASS "stub venue" >/dev/null; }

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

# --- dks_resolve_ev: PROVENANCE of every evidence item -----------------------
# FALSE-GREEN #4 (unit level): an operator-authored file and a machine this
# harness inspected are two different kinds of thing, and the difference must
# be carried, not dropped.
dks_resolve_ev "$blob" node-a tailscale0
is "resolve_ev: inspection-pod value is OBSERVED" "$DKS_EV_SOURCE" "observed"
is "resolve_ev: observed value returned" "$DKS_EV_VALUE" "ipv4:100.64.0.9"

dks_resolve_ev "" node-a tailscale0
is "resolve_ev: file-only value is ATTESTED" "$DKS_EV_SOURCE" "attested"
is "resolve_ev: attested value returned" "$DKS_EV_VALUE" "ipv4:100.64.0.5"

# Observation always outranks attestation for the same key.
dks_resolve_ev "EV tailscale0=ipv4:100.64.7.7" node-a tailscale0
is "resolve_ev: observation wins over attestation" "$DKS_EV_SOURCE" "observed"
is "resolve_ev: observed value wins" "$DKS_EV_VALUE" "ipv4:100.64.7.7"

dks_resolve_ev "" node-a cni_confdir
is "resolve_ev: nothing anywhere -> none" "$DKS_EV_SOURCE" "none"
is "resolve_ev: nothing anywhere -> empty value" "$DKS_EV_VALUE" ""
dks_resolve_ev "" node-a cni_confdir && bad "resolve_ev: no evidence must return non-zero" || ok "resolve_ev: no evidence returns non-zero"

unset DKS_HOST_EVIDENCE
is "dks_file_ev no file -> empty" "$(dks_file_ev node-a tailscale0 || true)" ""
dks_resolve_ev "" node-a tailscale0
is "resolve_ev: no evidence file at all -> none" "$DKS_EV_SOURCE" "none"

# --- precondition vs substantive classification ------------------------------
# FALSE-GREEN #5: `distinct-pod-cidrs` is a PRECONDITION. It reads .spec.podCIDR
# out of the API server and nothing else — no probe pod, no host inspection, no
# packet — and distinct per-node podCIDRs are IPAM bookkeeping k3s assigns at
# registration under --allocate-node-cidrs. They stay true with flannel dead,
# tailscale0 absent and no byte able to cross between nodes. It was the ONLY
# substantive check that survived a degraded environment, which made it exactly
# the check that still passed when everything else blocked.
for p in preflight nodes-ready peer-venue distinct-pod-cidrs; do
    dks_is_precondition "$p" && ok "precondition: $p" || bad "precondition: $p"
done
for s in flannel-iface no-stale-conflist cross-node-pod-ip \
         service-clusterip cluster-dns logs-exec headlamp nanochat bashy-chunked; do
    dks_is_precondition "$s" && bad "substantive: $s must NOT be a precondition" || ok "substantive: $s"
done

# THE evidence rule, asserted directly: only a check that MOVED A PACKET or
# INSPECTED A HOST may count toward a green verdict.
for e in flannel-iface no-stale-conflist cross-node-pod-ip service-clusterip \
         cluster-dns logs-exec headlamp nanochat bashy-chunked; do
    dks_is_evidence "$e" && ok "evidence: $e" || bad "evidence: $e must count as evidence"
done
# Every precondition, and distinct-pod-cidrs above all, is NOT evidence.
for n in preflight nodes-ready peer-venue distinct-pod-cidrs; do
    dks_is_evidence "$n" && bad "not-evidence: $n must NOT count as evidence" || ok "not-evidence: $n"
done
# The classification is an ALLOWLIST, not a blacklist: a check nobody has
# classified contributes nothing to a green verdict. This is what stops the
# NEXT API-only check from inheriting the saturating role by being forgotten.
dks_is_evidence some-future-api-only-check \
    && bad "an unclassified check must NOT be evidence by default" \
    || ok "unclassified check defaults to NOT evidence (allowlist, not blacklist)"

# --- dks_record / dks_summary ----------------------------------------------
reset
line="$(dks_record demo PASS all good)"
is "record emits one CHECK line" "$line" "CHECK demo PASS all good"
# Tally must be asserted outside a command substitution — $( ) forks a subshell,
# so the counter mutation inside it would not reach us.
reset
dks_record cross-node-pod-ip PASS all good >/dev/null
is "record tallies pass" "$DKS_PASS_COUNT" "1"
is "record tallies a substantive pass" "$DKS_SUBSTANTIVE_PASS_COUNT" "1"

# FALSE-GREEN #5 (unit level): an API-only check's PASS is tallied as a pass but
# must NOT be a substantive pass — it cannot make a run green on its own.
reset
dks_record distinct-pod-cidrs PASS "2 distinct podCIDRs" >/dev/null
is "distinct-pod-cidrs tallies as pass" "$DKS_PASS_COUNT" "1"
is "distinct-pod-cidrs is NOT a substantive pass" "$DKS_SUBSTANTIVE_PASS_COUNT" "0"

# An unclassified check likewise cannot saturate the gate.
reset
dks_record some-future-api-only-check PASS "looks convincing" >/dev/null
is "unclassified pass is NOT a substantive pass" "$DKS_SUBSTANTIVE_PASS_COUNT" "0"

# FALSE-GREEN #1 (unit level): a precondition PASS is NOT evidence about the
# slice under test and must not satisfy the "something passed" requirement.
reset
dks_record nodes-ready PASS "2 nodes" >/dev/null
is "precondition pass tallies as pass" "$DKS_PASS_COUNT" "1"
is "precondition pass is NOT a substantive pass" "$DKS_SUBSTANTIVE_PASS_COUNT" "0"

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

# The same invariant for ATTESTED: an operator-attested result is neither a
# pass nor a failure, and must never be renderable as PASS.
reset
dks_record attestme ATTESTED "operator said so" >/dev/null
is "attested does not count as pass" "$DKS_PASS_COUNT" "0"
is "attested does not count as substantive pass" "$DKS_SUBSTANTIVE_PASS_COUNT" "0"
is "attested does not count as fail" "$DKS_FAIL_COUNT" "0"
is "attested counts as attested" "$DKS_ATTESTED_COUNT" "1"
case "${DKS_RESULTS[0]}" in
    *" PASS "*) bad "ATTESTED result must never contain PASS" "${DKS_RESULTS[0]}" ;;
    "CHECK attestme ATTESTED operator said so") ok "ATTESTED recorded verbatim, never PASS" ;;
    *) bad "attested line shape" "${DKS_RESULTS[0]}" ;;
esac

# An unknown status must degrade to FAIL, not be treated as success.
reset
dks_record weird MAYBE something >/dev/null
is "unknown status degrades to fail" "$DKS_FAIL_COUNT" "1"
is "unknown status is not a pass" "$DKS_PASS_COUNT" "0"

# --- exit-status contract ---------------------------------------------------
reset
venue_ok; dks_record cross-node-pod-ip PASS x >/dev/null; dks_record b BLOCKED y >/dev/null
sum="$(dks_summary)"; rc=$?
is "venue asserted + substantive pass, no failures -> summary rc 0" "$rc" "0"
case "$sum" in *"pass=2 fail=0 blocked=1"*) ok "summary tallies" ;; *) bad "summary tallies" "$sum" ;; esac
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

# FALSE-GREEN #1 (summary level): preconditions passing while every
# substantive check is BLOCKED proves nothing. Must be INCONCLUSIVE, rc 2.
reset
dks_record nodes-ready PASS "2 nodes" >/dev/null
dks_record peer-venue PASS "peer plane" >/dev/null
dks_record flannel-iface BLOCKED "no evidence" >/dev/null
dks_record cross-node-pod-ip BLOCKED "no probe" >/dev/null
sum="$(dks_summary)"; rc=$?
is "preconditions-only -> rc 2 (INCONCLUSIVE)" "$rc" "2"
case "$sum" in *"only precondition checks passed"*) ok "preconditions-only names the reason" ;; *) bad "preconditions-only wording" "$sum" ;; esac
case "$sum" in *"RESULT OK"*) bad "preconditions-only must never say OK" "$sum" ;; *) ok "preconditions-only never says OK" ;; esac

# FALSE-GREEN #4 (summary level): operator attestation alone is not proof.
reset
dks_record nodes-ready PASS "2 nodes" >/dev/null
dks_record flannel-iface ATTESTED "operator file" >/dev/null
sum="$(dks_summary)"; rc=$?
is "attested-only -> rc 2 (INCONCLUSIVE)" "$rc" "2"
case "$sum" in *"only operator-attested evidence"*) ok "attested-only names the reason" ;; *) bad "attested-only wording" "$sum" ;; esac
case "$sum" in *"attested=1"*) ok "summary tallies attested separately" ;; *) bad "summary tallies attested separately" "$sum" ;; esac

# An attested check alongside a real substantive PASS is still green — but the
# summary must say the attested item was NOT counted as proof.
reset
venue_ok
dks_record cross-node-pod-ip PASS "0% loss" >/dev/null
dks_record flannel-iface ATTESTED "operator file" >/dev/null
sum="$(dks_summary)"; rc=$?
is "substantive pass + attested -> rc 0" "$rc" "0"
case "$sum" in *"operator-attested and NOT counted as proof"*) ok "OK line disclaims the attested item" ;; *) bad "OK line disclaims the attested item" "$sum" ;; esac

# --- FALSE-GREEN #5: the venue assertion is BINDING --------------------------
# `peer-venue` is the entire point of a *peer*-DKS gate. Anything other than an
# observed PASS -- FAIL, BLOCKED, or NOT SELECTED -- caps the verdict. It must
# not be possible to reach RESULT OK without the venue positively asserted.

# BLOCKED venue: a real substantive pass is NOT enough.
reset
dks_record peer-venue BLOCKED "annotation query could not run" >/dev/null
dks_record cross-node-pod-ip PASS "0% packet loss" >/dev/null
sum="$(dks_summary)"; rc=$?
is "venue BLOCKED caps a substantive pass -> rc 2" "$rc" "2"
case "$sum" in *"RESULT OK"*) bad "venue BLOCKED must never say OK" "$sum" ;; *) ok "venue BLOCKED never says OK" ;; esac
case "$sum" in *"venue could not be asserted"*) ok "venue BLOCKED names the reason" ;; *) bad "venue BLOCKED wording" "$sum" ;; esac

# NOT SELECTED venue: same cap. This is the DKS_ONLY bypass.
reset
dks_record cross-node-pod-ip PASS "0% packet loss" >/dev/null
sum="$(dks_summary)"; rc=$?
is "venue never run caps a substantive pass -> rc 2" "$rc" "2"
case "$sum" in *"RESULT OK"*) bad "unasserted venue must never say OK" "$sum" ;; *) ok "unasserted venue never says OK" ;; esac
case "$sum" in *"never asserted"*) ok "unasserted venue names the reason" ;; *) bad "unasserted venue wording" "$sum" ;; esac

# The venue status is carried in the SUMMARY line, so a log reader can see
# which of the two required conditions was missing without re-deriving it.
case "$sum" in *"venue=NOT-RUN"*) ok "summary reports venue=NOT-RUN" ;; *) bad "summary reports venue" "$sum" ;; esac

# The positive control: venue asserted AND a substantive pass -> and ONLY then
# is the run green. Without this the suite could not tell "correctly strict"
# from "can never be green".
reset
venue_ok; dks_record cross-node-pod-ip PASS "0% packet loss" >/dev/null
sum="$(dks_summary)"; rc=$?
is "venue PASS + substantive pass -> rc 0" "$rc" "0"
case "$sum" in *"venue=PASS"*) ok "summary reports venue=PASS" ;; *) bad "summary reports venue=PASS" "$sum" ;; esac
case "$sum" in *"peer venue asserted by peer-venue"*) ok "OK line states the venue was asserted" ;; *) bad "OK line states the venue was asserted" "$sum" ;; esac

# The venue check cannot pull its own weight either: venue PASS + an API-only
# pass is still nothing proven (FALSE-GREEN #5 and the venue rule together).
reset
venue_ok; dks_record nodes-ready PASS "2 nodes" >/dev/null
dks_record distinct-pod-cidrs PASS "2 distinct podCIDRs" >/dev/null
sum="$(dks_summary)"; rc=$?
is "venue PASS + only API-only passes -> rc 2" "$rc" "2"
case "$sum" in *"RESULT OK"*) bad "API-only passes must never say OK" "$sum" ;; *) ok "API-only passes never say OK" ;; esac

# Zero checks at all is the same absence-of-evidence case.
reset
sum="$(dks_summary)"; rc=$?
is "no check ran -> rc 2 (INCONCLUSIVE)" "$rc" "2"
case "$sum" in *"INCONCLUSIVE (no check ran"*) ok "no-check-ran names the reason" ;; *) bad "no-check-ran wording" "$sum" ;; esac

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
    # `kubectl exec` — the stub answers as the remote command would, INCLUDING
    # its return code, so tests can drive the rc/content combinations the
    # harness must distinguish.
    *" exec "*)
        case "$*" in
            *httpd*) exit 0 ;;
            *ping*)     cat "${DKS_STUB_PING_OUT:-/dev/null}"; exit "${DKS_STUB_PING_RC:-0}" ;;
            *nslookup*) cat "${DKS_STUB_DNS_OUT:-/dev/null}"; exit "${DKS_STUB_DNS_RC:-0}" ;;
            *wget*)
                if [ "${DKS_STUB_WGET_HOSTNAME:-0}" = "1" ]; then
                    # Emulate `httpd -h /etc` on the BACKEND pod: GET /hostname
                    # returns that pod's own hostname (kubelet sets it to the
                    # pod name). The exec runs FROM the "-a" probe, so derive
                    # the "-b" backend name from the argv.
                    for a in "$@"; do
                        case "$a" in *-a) echo "${a%-a}-b"; break ;; esac
                    done
                else
                    cat "${DKS_STUB_WGET_OUT:-/dev/null}"
                fi
                exit "${DKS_STUB_WGET_RC:-0}" ;;
            *dks-exec-ok*) cat "${DKS_STUB_EXECO:-/dev/null}"; exit "${DKS_STUB_EXEC_RC:-0}" ;;
        esac
        exit 0 ;;
    # Suffix match, not substring: "-o jsonpath=..." also contains "-o json",
    # and swallowing a jsonpath call would corrupt a fixture answer.
    *"-o json") echo '{"items":[]}'; exit 0 ;;
    # The three node queries can be made to FAIL (rc + stderr, no stdout), which
    # is what an RBAC denial or a transient API error actually looks like. A
    # harness that discards rc cannot tell this from a healthy cluster with
    # nothing to report -- that is the defect these knobs exist to exercise.
    *custom-columns*)
        if [ "${DKS_STUB_CIDRS_RC:-0}" != "0" ]; then
            echo 'Error from server (Forbidden): nodes is forbidden' >&2; exit "$DKS_STUB_CIDRS_RC"
        fi
        cat "${DKS_STUB_CIDRS:-/dev/null}"; exit 0 ;;
    *public-ip*)
        if [ "${DKS_STUB_ANN_RC:-0}" != "0" ]; then
            echo 'Error from server (Forbidden): nodes is forbidden' >&2; exit "$DKS_STUB_ANN_RC"
        fi
        cat "${DKS_STUB_ANN:-/dev/null}"; exit 0 ;;
    *kubeletVersion*Ready*)
        if [ "${DKS_STUB_NODES_RC:-0}" != "0" ]; then
            echo 'Error from server (Forbidden): nodes is forbidden' >&2; exit "$DKS_STUB_NODES_RC"
        fi
        cat "${DKS_STUB_NODES:-/dev/null}"; exit 0 ;;
    *"{.items[0].status.phase}") echo "${DKS_STUB_HL_PHASE:-}"; exit 0 ;;
    *"{.spec.clusterIP}") echo "${DKS_STUB_SVC_IP:-}"; exit 0 ;;
    *"{.spec.ports[0].port}") echo "${DKS_STUB_SVC_PORT:-}"; exit 0 ;;
    *"{.status.podIP}") echo "${DKS_STUB_POD_IP:-}"; exit 0 ;;
    *"{.status.phase}") echo "${DKS_STUB_POD_PHASE:-Succeeded}"; exit 0 ;;
    *" logs "*)
        case "$*" in
            *"-insp-"*"-0") cat "${DKS_STUB_EV_A:-/dev/null}"; exit 0 ;;
            *"-insp-"*"-1") cat "${DKS_STUB_EV_B:-/dev/null}"; exit 0 ;;
        esac
        cat "${DKS_STUB_LOGS:-/dev/null}"; exit "${DKS_STUB_LOGS_RC:-0}" ;;
    *"rollout status deployment/"*) exit "${DKS_STUB_ROLLOUT_RC:-0}" ;;
    *"app="*".spec.nodeName"*) cat "${DKS_STUB_NC_NODES:-/dev/null}"; exit 0 ;;
    *"app="*"status.phase=Running"*) cat "${DKS_STUB_NC_READY:-/dev/null}"; exit 0 ;;
    *"app="*"waiting.reason"*) cat "${DKS_STUB_NC_PULL:-/dev/null}"; exit 0 ;;
    *"job-name="*"waiting.reason"*) cat "${DKS_STUB_BJ_PULL:-/dev/null}"; exit 0 ;;
    *"job-name="*".spec.nodeName"*) cat "${DKS_STUB_BJ_NODES:-/dev/null}"; exit 0 ;;
    *"get job "*"{.status.succeeded}") echo "${DKS_STUB_BJ_SUCC:-}"; exit 0 ;;
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
# No node carries the annotation at all: flannel is not running anywhere. This
# is the shape of a cloudbox-hosted (non-peer) cluster.
printf 'node-a\nnode-b\n' >"$FIX/ann-none"
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
# Operator host-evidence files (ATTESTED, never observed).
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
printf 'ImagePullBackOff\n' >"$FIX/pull-issue"

# exec-output fixtures.
printf '3 packets transmitted, 3 packets received, 0%% packet loss\n' >"$FIX/ping-ok"
printf '3 packets transmitted, 0 packets received, 100%% packet loss\n' >"$FIX/ping-loss"
# busybox's own words when the pod network is broken. On NO blacklist, which is
# exactly why a negative blacklist scored it as a PASS.
printf 'wget: bad address: Network is unreachable\n' >"$FIX/wget-unreachable"
printf 'some-other-endpoint\n' >"$FIX/wget-wrong-body"
printf 'HTTP/1.1 404 Not Found\n' >"$FIX/wget-http404"
printf 'dks-alive\ndks-alive\n' >"$FIX/logs-alive"
printf 'dks-exec-ok\n' >"$FIX/exec-ok"
printf 'Name:\tstub\nAddress: 10.43.9.9\n' >"$FIX/dns-ok"
printf '** server can not find it: NXDOMAIN\n' >"$FIX/dns-nxdomain"

stub_reset() {
    STUB_NODES="$FIX/two-nodes"; STUB_CIDRS=/dev/null; STUB_ANN=/dev/null
    STUB_EV_A=/dev/null; STUB_EV_B=/dev/null; STUB_LOG=""
    STUB_APPLY_RC=0; STUB_PHASE=Succeeded; STUB_DEBUG=0; STUB_HOSTEV=""
    STUB_TIMEOUT=5
    STUB_ROLLOUT_RC=0; STUB_NC_NODES=/dev/null; STUB_NC_READY=/dev/null
    STUB_NC_PULL=/dev/null; STUB_NANOCHAT_IMG="stub/nanochat:latest"
    STUB_READY_A=false; STUB_READY_B=false
    STUB_SVC_IP=""; STUB_SVC_PORT=""; STUB_POD_IP=""
    STUB_PING_OUT=/dev/null; STUB_PING_RC=0
    STUB_WGET_OUT=/dev/null; STUB_WGET_RC=0; STUB_WGET_HOSTNAME=0
    STUB_DNS_OUT=/dev/null; STUB_DNS_RC=0
    STUB_LOGS=/dev/null; STUB_LOGS_RC=0; STUB_EXECO=/dev/null; STUB_EXEC_RC=0
    STUB_HL_NS=""; STUB_HL_SVC=""; STUB_HL_PHASE=""
    STUB_BASHY_IMG=""; STUB_BJ_SUCC=""; STUB_BJ_NODES=/dev/null; STUB_BJ_PULL=/dev/null
    # Query-failure knobs: 0 = the query answers, non-zero = it could not run.
    STUB_NODES_RC=0; STUB_ANN_RC=0; STUB_CIDRS_RC=0
}

# run_case <DKS_ONLY> — executes the real runner; sets RH_OUT / RH_RC.
run_case() {
    RH_OUT="$(PATH="$STUB_PATH" DKS_NAMESPACE=default DKS_ONLY="$1" \
        DKS_STUB_NODES="$STUB_NODES" DKS_STUB_CIDRS="$STUB_CIDRS" \
        DKS_STUB_ANN="$STUB_ANN" DKS_STUB_EV_A="$STUB_EV_A" \
        DKS_STUB_NODES_RC="$STUB_NODES_RC" DKS_STUB_ANN_RC="$STUB_ANN_RC" \
        DKS_STUB_CIDRS_RC="$STUB_CIDRS_RC" \
        DKS_STUB_EV_B="$STUB_EV_B" DKS_STUB_LOG="$STUB_LOG" \
        DKS_STUB_APPLY_RC="$STUB_APPLY_RC" DKS_STUB_POD_PHASE="$STUB_PHASE" \
        DKS_ALLOW_NODE_DEBUG="$STUB_DEBUG" DKS_HOST_EVIDENCE="$STUB_HOSTEV" \
        DKS_TIMEOUT="$STUB_TIMEOUT" \
        DKS_STUB_ROLLOUT_RC="$STUB_ROLLOUT_RC" DKS_STUB_NC_NODES="$STUB_NC_NODES" \
        DKS_STUB_NC_READY="$STUB_NC_READY" DKS_STUB_NC_PULL="$STUB_NC_PULL" \
        DKS_NANOCHAT_IMAGE="$STUB_NANOCHAT_IMG" \
        DKS_STUB_READY_A="$STUB_READY_A" DKS_STUB_READY_B="$STUB_READY_B" \
        DKS_STUB_SVC_IP="$STUB_SVC_IP" DKS_STUB_SVC_PORT="$STUB_SVC_PORT" \
        DKS_STUB_POD_IP="$STUB_POD_IP" \
        DKS_STUB_PING_OUT="$STUB_PING_OUT" DKS_STUB_PING_RC="$STUB_PING_RC" \
        DKS_STUB_WGET_OUT="$STUB_WGET_OUT" DKS_STUB_WGET_RC="$STUB_WGET_RC" \
        DKS_STUB_WGET_HOSTNAME="$STUB_WGET_HOSTNAME" \
        DKS_STUB_DNS_OUT="$STUB_DNS_OUT" DKS_STUB_DNS_RC="$STUB_DNS_RC" \
        DKS_STUB_LOGS="$STUB_LOGS" DKS_STUB_LOGS_RC="$STUB_LOGS_RC" \
        DKS_STUB_EXECO="$STUB_EXECO" DKS_STUB_EXEC_RC="$STUB_EXEC_RC" \
        DKS_HEADLAMP_NS="$STUB_HL_NS" DKS_HEADLAMP_SVC="$STUB_HL_SVC" \
        DKS_STUB_HL_PHASE="$STUB_HL_PHASE" \
        DKS_BASHY_IMAGE="$STUB_BASHY_IMG" DKS_STUB_BJ_SUCC="$STUB_BJ_SUCC" \
        DKS_STUB_BJ_NODES="$STUB_BJ_NODES" DKS_STUB_BJ_PULL="$STUB_BJ_PULL" \
        "$BASH_BIN" "$HARNESS" 2>&1)"
    RH_RC=$?
}

# run_case_on_peer_venue <checks> — as run_case, but ALSO selects `peer-venue`
# and (unless the test pinned its own) supplies a venue-valid annotation
# fixture. Every test whose subject is some OTHER check but which expects a
# GREEN verdict has to assert the venue too, because the venue assertion is
# binding. That is the rule working as designed, not a workaround: a green
# verdict is exactly "we know where we are AND we observed something here".
run_case_on_peer_venue() {
    [ "$STUB_ANN" = "/dev/null" ] && STUB_ANN="$FIX/ann-good"
    run_case "peer-venue,$1"
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
# Every documented code is asserted against the real process, not its stdout.
stub_reset; run_case nonexistent-check
expect "runner: no check ran" 2 "RESULT INCONCLUSIVE (no check ran"

stub_reset; run_case no-stale-conflist
expect "runner: all BLOCKED" 2 "RESULT INCONCLUSIVE (no check passed"

# FALSE-GREEN #1, end to end: `nodes-ready` is the one check that is trivially
# true on any 2-node cluster. Before the fix it alone satisfied the ">=1 PASS"
# gate, so a run with EVERY substantive check BLOCKED still printed RESULT OK
# and exited 0 -- a gate reading only $? scored "we proved nothing" as proof.
stub_reset; STUB_NANOCHAT_IMG=""
run_case nodes-ready,flannel-iface,no-stale-conflist,nanochat,bashy-chunked
expect "runner: precondition PASS + every substantive check BLOCKED" 2 \
    "RESULT INCONCLUSIVE (only precondition checks passed" "RESULT OK"
case "$RH_OUT" in *"CHECK nodes-ready PASS"*) ok "runner: nodes-ready did PASS (the gate is not passing by accident)" ;; *) bad "runner: nodes-ready did PASS" "$RH_OUT" ;; esac

# Same shape with the venue check also passing: two preconditions green, every
# substantive check BLOCKED, still nothing proven.
stub_reset; STUB_ANN="$FIX/ann-good"; STUB_NANOCHAT_IMG=""
run_case nodes-ready,peer-venue,no-stale-conflist,nanochat
expect "runner: both preconditions PASS, substantive BLOCKED" 2 \
    "RESULT INCONCLUSIVE (only precondition checks passed" "RESULT OK"

# A one-node venue is a MISSING PRECONDITION (nothing was tested), so it
# BLOCKs -> INCONCLUSIVE. Scoring it FAIL would call an absent venue a defect.
stub_reset; STUB_NODES="$FIX/one-node"; run_case nodes-ready
expect "runner: one-node venue -> BLOCKED, not FAIL" 2 "CHECK nodes-ready BLOCKED" "CHECK nodes-ready FAIL"

# A real FAIL still outranks INCONCLUSIVE.
stub_reset; STUB_CIDRS="$FIX/cidrs-ready-dup"
run_case distinct-pod-cidrs,no-stale-conflist
expect "runner: FAIL outranks INCONCLUSIVE" 1 "RESULT FAIL"

# THE END-TO-END POSITIVE CONTROL. Exit 0 requires BOTH conditions and this is
# the only shape that satisfies them: the venue positively asserted, AND a check
# that actually moved a packet observed to pass. Without this test the suite
# could not distinguish "correctly strict" from "can never be green", which
# would be its own false result.
stub_reset; STUB_ANN="$FIX/ann-good"
STUB_READY_A=true; STUB_READY_B=true; STUB_TIMEOUT=1
STUB_POD_IP="10.42.1.7"; STUB_PING_OUT="$FIX/ping-ok"; STUB_PING_RC=0
run_case peer-venue,cross-node-pod-ip
expect "runner: venue asserted + a packet moved -> the ONLY green shape" 0 "RESULT OK"
case "$RH_OUT" in *"venue=PASS"*) ok "runner: green run reports venue=PASS" ;; *) bad "runner: green run reports venue=PASS" "$RH_OUT" ;; esac
case "$RH_OUT" in *"substantive_pass=1"*) ok "runner: green run reports substantive_pass=1" ;; *) bad "runner: green run reports substantive_pass=1" "$RH_OUT" ;; esac

# FALSE-GREEN #5, end to end: the SAME degraded cluster -- API server healthy,
# venue fine, every packet-moving and host-inspecting check BLOCKED -- used to
# exit 0 off distinct-pod-cidrs alone (SUMMARY pass=2 fail=0 blocked=10 ->
# RESULT OK). An API-only read must never carry the verdict.
stub_reset; STUB_ANN="$FIX/ann-good"; STUB_CIDRS="$FIX/cidrs-excluded-dups"
STUB_NANOCHAT_IMG=""; STUB_TIMEOUT=1
run_case nodes-ready,peer-venue,distinct-pod-cidrs,flannel-iface,no-stale-conflist,cross-node-pod-ip,nanochat,bashy-chunked
expect "runner: API-only pass cannot saturate a degraded run" 2 \
    "CHECK distinct-pod-cidrs PASS" "RESULT OK"
case "$RH_OUT" in *"substantive_pass=0"*) ok "runner: API-only pass is not substantive" ;; *) bad "runner: API-only pass is not substantive" "$RH_OUT" ;; esac

out="$(PATH="$FIX/nokubectl" DKS_NAMESPACE=default DKS_ONLY=nodes-ready \
    "$BASH_BIN" "$HARNESS" 2>&1)"; rc=$?
is "runner: kubectl absent -> exit 2" "$rc" "2"
case "$out" in *"CHECK preflight BLOCKED"*) ok "kubectl absent -> preflight BLOCKED" ;; *) bad "kubectl absent wording" "$out" ;; esac

# --- FALSE-GREEN #3: the peer venue was never asserted -----------------------
# Nothing in the pre-corrective harness checked WHERE it was running, so it
# would emit a green verdict against ANY 2-node cluster -- including the
# cloudbox-hosted one, which runs no flannel at all and is not the plane this
# gate exists to accept.
stub_reset; STUB_ANN="$FIX/ann-none"
run_case nodes-ready,peer-venue
expect "venue: no flannel anywhere -> FAIL (not a peer-hosted plane)" 1 \
    "CHECK peer-venue FAIL" "RESULT OK"
case "$RH_OUT" in *"no-flannel-public-ip-annotation"*) ok "venue: names the missing annotation per node" ;; *) bad "venue: names the missing annotation" "$RH_OUT" ;; esac

# Flannel present but its underlay is not the tailnet: still not ADR Option A.
stub_reset; STUB_ANN="$FIX/ann-bad-b"
run_case peer-venue
expect "venue: non-tailnet flannel underlay -> FAIL" 1 "CHECK peer-venue FAIL" "CHECK peer-venue PASS"
case "$RH_OUT" in *"flannel-public-ip-not-tailnet(10.0.0.6)"*) ok "venue: names the offending address" ;; *) bad "venue: names the offending address" "$RH_OUT" ;; esac

# One node annotated, one not: a mixed venue is not a coherent peer plane.
stub_reset; STUB_ANN="$FIX/ann-missing-b"
run_case peer-venue
expect "venue: partially annotated -> FAIL" 1 "CHECK peer-venue FAIL" "CHECK peer-venue PASS"

# The positive case: every Ready node carries a tailnet flannel public-ip.
stub_reset; STUB_ANN="$FIX/ann-good"
run_case peer-venue
expect "venue: peer-hosted flannel plane -> PASS" 2 "CHECK peer-venue PASS" "RESULT OK"
case "$RH_OUT" in *"only precondition checks passed"*) ok "venue: a venue PASS alone is still INCONCLUSIVE" ;; *) bad "venue: a venue PASS alone is still INCONCLUSIVE" "$RH_OUT" ;; esac

# An unanswerable query is missing evidence, not a contradiction -> BLOCKED.
stub_reset; STUB_ANN=/dev/null
run_case peer-venue
expect "venue: annotation query returned nothing -> BLOCKED" 2 "CHECK peer-venue BLOCKED" "CHECK peer-venue FAIL"

stub_reset; STUB_NODES=/dev/null
run_case peer-venue
expect "venue: no Ready node at all -> BLOCKED" 2 "CHECK peer-venue BLOCKED" "CHECK peer-venue FAIL"

# --- FALSE-GREEN #5: DKS_ONLY could bypass the venue assertion entirely -------
# THE two-invocation proof, on ONE cluster that is demonstrably not peer-hosted
# (no flannel anywhere). With `peer-venue` selected the harness said so and
# exited 1; with it merely not selected the same cluster exited 0. A cluster
# the harness ITSELF declares non-peer must never pass the gate, and which
# checks the operator happened to select cannot be what decides that.
stub_reset; STUB_ANN="$FIX/ann-none"; STUB_CIDRS="$FIX/cidrs-excluded-dups"
run_case nodes-ready,peer-venue,distinct-pod-cidrs
expect "venue bypass: WITH peer-venue -> FAIL (not a peer plane)" 1 "CHECK peer-venue FAIL"
VENUE_WITH_RC="$RH_RC"

stub_reset; STUB_ANN="$FIX/ann-none"; STUB_CIDRS="$FIX/cidrs-excluded-dups"
run_case nodes-ready,distinct-pod-cidrs
expect "venue bypass: WITHOUT peer-venue -> still not green" 2 \
    "the peer venue was never asserted" "RESULT OK"
case "$RH_RC" in 0) bad "venue bypass: deselecting peer-venue must not reach exit 0" "$RH_OUT" ;; *) ok "venue bypass: deselecting peer-venue cannot reach exit 0" ;; esac
# Neither invocation may be green: the two must not disagree about the verdict.
if [ "$VENUE_WITH_RC" != "0" ] && [ "$RH_RC" != "0" ]; then
    ok "venue bypass: both invocations agree the cluster is not acceptable"
else
    bad "venue bypass: invocations disagree" "with=$VENUE_WITH_RC without=$RH_RC"
fi

# A venue that could not be asserted caps even a genuine packet-moving PASS.
stub_reset; STUB_ANN_RC=1
STUB_READY_A=true; STUB_READY_B=true; STUB_TIMEOUT=1
STUB_POD_IP="10.42.1.7"; STUB_PING_OUT="$FIX/ping-ok"; STUB_PING_RC=0
run_case peer-venue,cross-node-pod-ip
expect "venue BLOCKED caps a real cross-node PASS" 2 "CHECK cross-node-pod-ip PASS" "RESULT OK"
case "$RH_OUT" in *"venue could not be asserted"*) ok "venue BLOCKED cap names the reason" ;; *) bad "venue BLOCKED cap names the reason" "$RH_OUT" ;; esac

# --- unchecked rc: "could not run" vs "ran and found nothing" -----------------
# A query whose rc is discarded turns an RBAC denial into a silent BLOCK with a
# confident, WRONG diagnosis. Each site must name the transport failure instead
# of reporting a fact about the cluster it never observed.
stub_reset; STUB_ANN_RC=1
run_case peer-venue
expect "rc: denied annotation query -> BLOCKED naming the failure" 2 \
    "the node-annotation query COULD NOT RUN" "CHECK peer-venue FAIL"
case "$RH_OUT" in *"Forbidden"*) ok "rc: venue BLOCK quotes the server error" ;; *) bad "rc: venue BLOCK quotes the server error" "$RH_OUT" ;; esac
# ...and it must NOT be reported as "the nodes carry no flannel annotation".
case "$RH_OUT" in *"no-flannel-public-ip-annotation"*) bad "rc: denied query must not read as 'no annotation'" "$RH_OUT" ;; *) ok "rc: denied query is not read as 'no annotation'" ;; esac

stub_reset; STUB_NODES_RC=1
run_case nodes-ready,peer-venue
expect "rc: denied node-list query -> nodes-ready BLOCKED naming the failure" 2 \
    "the node-list query COULD NOT RUN" "CHECK nodes-ready PASS"
case "$RH_OUT" in *"NOT 'zero Ready nodes'"*) ok "rc: node-list BLOCK distinguishes itself from 'found 0'" ;; *) bad "rc: node-list BLOCK distinguishes itself from 'found 0'" "$RH_OUT" ;; esac

stub_reset; STUB_CIDRS_RC=1
run_case distinct-pod-cidrs
expect "rc: denied podCIDR query -> BLOCKED naming the failure" 2 \
    "the node podCIDR query COULD NOT RUN" "CHECK distinct-pod-cidrs PASS"

stub_reset; STUB_ANN_RC=1
run_case flannel-iface
# The anti-needle is the FULL wrong diagnosis ("flannel is not running on the
# selected nodes"), not the fragment: the corrected BLOCKED message quotes the
# fragment back to say it is NOT what happened.
expect "rc: denied annotation query -> flannel-iface BLOCKED, not 'flannel not running'" 2 \
    "the node-annotation query COULD NOT RUN" "flannel is not running on the selected nodes"

# THE regression this whole corrective exists to prevent: a healthy API server,
# every real check denied or blocked, and the run still exiting 0.
stub_reset; STUB_ANN_RC=1; STUB_CIDRS="$FIX/cidrs-excluded-dups"
STUB_NANOCHAT_IMG=""; STUB_TIMEOUT=1
run_case nodes-ready,peer-venue,distinct-pod-cidrs,flannel-iface,no-stale-conflist,cross-node-pod-ip,service-clusterip,cluster-dns,logs-exec,headlamp,nanochat,bashy-chunked
expect "the pass=N fail=0 blocked=many -> exit 0 shape is unreachable" 2 "" "RESULT OK"
case "$RH_RC" in 0) bad "degraded-cluster run must never exit 0" "$RH_OUT" ;; *) ok "degraded-cluster run does not exit 0" ;; esac

# --- flannel-iface: annotation alone can never PASS ---------------------------
stub_reset; STUB_ANN="$FIX/ann-good"
run_case flannel-iface
expect "flannel: annotation-only" 2 "CHECK flannel-iface BLOCKED" "CHECK flannel-iface PASS"

# Inspection permitted but pods return no evidence: still BLOCKED, never PASS/FAIL.
stub_reset; STUB_ANN="$FIX/ann-good"; STUB_DEBUG=1
run_case flannel-iface
expect "flannel: inspection yields no evidence" 2 "CHECK flannel-iface BLOCKED" "CHECK flannel-iface FAIL"

# --- flannel-iface: full OBSERVED evidence on BOTH nodes is the only PASS -----
# ev-good-b (not ev-good) for NODE_B: ann-good gives node-a=100.64.0.5 and
# node-b=100.64.0.6, so each node's tailscale0 evidence must match ITS OWN
# annotation, not just be independently well-formed.
stub_reset; STUB_ANN="$FIX/ann-good"; STUB_DEBUG=1
STUB_EV_A="$FIX/ev-good"; STUB_EV_B="$FIX/ev-good-b"; STUB_LOG="$FIX/log-flannel-pass"
run_case_on_peer_venue flannel-iface
expect "flannel: full observed evidence both nodes" 0 "CHECK flannel-iface PASS"
case "$RH_OUT" in *"annotation-equals-tailscale0"*) ok "flannel: equality evidence named on PASS" ;; *) bad "flannel: equality evidence named on PASS" "$RH_OUT" ;; esac
# Every item on a PASS record must be marked observed — a reader must be able
# to tell provenance from the record alone.
case "$RH_OUT" in *"argv-ok(observed)"*) ok "flannel: PASS record marks argv as observed" ;; *) bad "flannel: PASS record marks argv as observed" "$RH_OUT" ;; esac
case "$RH_OUT" in *"(attested)"*) bad "flannel: observed PASS must carry no attested item" "$RH_OUT" ;; *) ok "flannel: observed PASS carries no attested item" ;; esac

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

# --- FALSE-GREEN #4: operator-supplied evidence rendered as observed ----------
# DKS_HOST_EVIDENCE is a text file a human typed. Before the fix it produced a
# full `CHECK flannel-iface PASS` -- indistinguishable, in the record and in
# the exit code, from a PASS backed by machines this harness actually
# inspected. No machine was inspected here at all.
stub_reset; STUB_ANN="$FIX/ann-good"; STUB_HOSTEV="$FIX/hostev-both"
run_case flannel-iface
expect "flannel: DKS_HOST_EVIDENCE both nodes -> ATTESTED, never PASS" 2 \
    "CHECK flannel-iface ATTESTED" "CHECK flannel-iface PASS"
case "$RH_OUT" in *"NOT observed by this harness"*) ok "flannel: attested record says nothing was inspected" ;; *) bad "flannel: attested record says nothing was inspected" "$RH_OUT" ;; esac
case "$RH_OUT" in *"RESULT OK"*) bad "flannel: attested-only run must not say OK" "$RH_OUT" ;; *) ok "flannel: attested-only run does not say OK" ;; esac

# Mixed provenance: node-a observed via an inspection pod, node-b only in the
# operator's file. One attested item is enough to demote the whole record.
stub_reset; STUB_ANN="$FIX/ann-good"; STUB_DEBUG=1
STUB_EV_A="$FIX/ev-good"; STUB_EV_B=/dev/null; STUB_HOSTEV="$FIX/hostev-both"
run_case flannel-iface
expect "flannel: mixed observed/attested -> ATTESTED" 2 "CHECK flannel-iface ATTESTED" "CHECK flannel-iface PASS"
case "$RH_OUT" in *"node-b:tailscale0"*) ok "flannel: attested record names WHICH node was not inspected" ;; *) bad "flannel: attested record names which node" "$RH_OUT" ;; esac

# Attested evidence still surfaces contradictions: an operator who reports a
# node WITHOUT the flag gets a FAIL, not a polite ATTESTED.
printf 'node-a k3s_argv=flannel-iface-tailscale0 tailscale0=ipv4:100.64.0.5\nnode-b k3s_argv=no-flannel-iface tailscale0=ipv4:100.64.0.6\n' >"$FIX/hostev-contra"
stub_reset; STUB_ANN="$FIX/ann-good"; STUB_HOSTEV="$FIX/hostev-contra"
run_case flannel-iface
expect "flannel: attested contradiction still FAILs" 1 "CHECK flannel-iface FAIL"

stub_reset; STUB_ANN="$FIX/ann-good"; STUB_HOSTEV="$FIX/hostev-one"
run_case flannel-iface
expect "flannel: DKS_HOST_EVIDENCE one node only" 2 "CHECK flannel-iface BLOCKED" "CHECK flannel-iface PASS"

# --- no-stale-conflist -------------------------------------------------------
stub_reset
run_case no-stale-conflist
expect "conflist: no inspection, no evidence" 2 "CHECK no-stale-conflist BLOCKED"

stub_reset; STUB_DEBUG=1; STUB_EV_A="$FIX/ev-good"; STUB_EV_B="$FIX/ev-good"
run_case_on_peer_venue no-stale-conflist
expect "conflist: clean on both nodes (observed)" 0 "CHECK no-stale-conflist PASS"
case "$RH_OUT" in *"(observed)"*) ok "conflist: PASS record marks provenance" ;; *) bad "conflist: PASS record marks provenance" "$RH_OUT" ;; esac

# Same clean result, but nobody inspected anything: ATTESTED, not PASS.
stub_reset; STUB_HOSTEV="$FIX/hostev-both"
run_case no-stale-conflist
expect "conflist: operator-attested clean -> ATTESTED, never PASS" 2 \
    "CHECK no-stale-conflist ATTESTED" "CHECK no-stale-conflist PASS"

stub_reset; STUB_DEBUG=1; STUB_EV_A="$FIX/ev-good"; STUB_EV_B="$FIX/ev-stale"
run_case no-stale-conflist
expect "conflist: stale conflist observed" 1 "CHECK no-stale-conflist FAIL"

stub_reset; STUB_DEBUG=1; STUB_EV_A="$FIX/ev-good"; STUB_EV_B="$FIX/ev-unreadable"
run_case no-stale-conflist
expect "conflist: unreadable stays BLOCKED" 2 "CHECK no-stale-conflist BLOCKED" "CHECK no-stale-conflist PASS"

# --- distinct-pod-cidrs: NotReady + virtual nodes excluded --------------------
# node-stale (NotReady, real) and node-virt (vknode) both collide with a Ready
# node's CIDR; both must be excluded, so the CHECK PASSes on the Ready pair.
# The RUN, however, is INCONCLUSIVE (exit 2): this check reads .spec.podCIDR
# from the API server and proves nothing about the slice, so its PASS cannot
# make the run green. Check-level PASS and run-level verdict are separate, and
# this test asserts both at once.
stub_reset; STUB_CIDRS="$FIX/cidrs-excluded-dups"
run_case distinct-pod-cidrs
expect "cidrs: NotReady/virtual excluded (check PASSes, run stays INCONCLUSIVE)" 2 \
    "CHECK distinct-pod-cidrs PASS" "RESULT OK"
case "$RH_OUT" in *"excluded from distinct-pod-cidrs (NotReady): node-stale"*) ok "cidrs: NotReady exclusion named" ;; *) bad "cidrs: NotReady exclusion named" "$RH_OUT" ;; esac
case "$RH_OUT" in *"excluded from distinct-pod-cidrs (virtual-kubelet): node-virt"*) ok "cidrs: virtual exclusion named" ;; *) bad "cidrs: virtual exclusion named" "$RH_OUT" ;; esac

# A duplicate between two READY kubelet-backed nodes must still FAIL.
stub_reset; STUB_CIDRS="$FIX/cidrs-ready-dup"
run_case distinct-pod-cidrs
expect "cidrs: Ready duplicate still fails" 1 "CHECK distinct-pod-cidrs FAIL"

# --- FALSE-GREEN #2: the exec return code was never read ---------------------
# `service-clusterip` PASSed on a NEGATIVE BLACKLIST: any output that did not
# mention refused|timed out|no route|error was success. busybox's own
# "Network is unreachable" is on no blacklist, so a completely broken pod
# network scored PASS -- with the error text quoted in the PASS line. The rc
# of `kubectl exec` was never looked at.
stub_reset; STUB_READY_A=true; STUB_READY_B=true; STUB_TIMEOUT=1
STUB_SVC_IP="10.43.0.9"; STUB_WGET_OUT="$FIX/wget-unreachable"; STUB_WGET_RC=1
run_case service-clusterip
expect "service-clusterip: unreachable network (rc!=0) -> FAIL" 1 \
    "CHECK service-clusterip FAIL" "CHECK service-clusterip PASS"
case "$RH_OUT" in *"exec rc=1"*) ok "service-clusterip: FAIL names the exec rc" ;; *) bad "service-clusterip: FAIL names the exec rc" "$RH_OUT" ;; esac

# rc 0 but the wrong bytes: some other endpoint answered. A negative blacklist
# calls that a PASS too. The positive content assertion does not.
stub_reset; STUB_READY_A=true; STUB_READY_B=true; STUB_TIMEOUT=1
STUB_SVC_IP="10.43.0.9"; STUB_WGET_OUT="$FIX/wget-wrong-body"; STUB_WGET_RC=0
run_case service-clusterip
expect "service-clusterip: rc 0 but wrong payload -> FAIL" 1 \
    "CHECK service-clusterip FAIL" "CHECK service-clusterip PASS"

# rc 0 AND the backend pod's own hostname: the only PASS.
stub_reset; STUB_READY_A=true; STUB_READY_B=true; STUB_TIMEOUT=1
STUB_SVC_IP="10.43.0.9"; STUB_WGET_HOSTNAME=1; STUB_WGET_RC=0
run_case_on_peer_venue service-clusterip
expect "service-clusterip: backend hostname returned -> PASS" 0 "CHECK service-clusterip PASS"
case "$RH_OUT" in *"backend pod's own hostname"*) ok "service-clusterip: PASS states what was proven" ;; *) bad "service-clusterip: PASS states what was proven" "$RH_OUT" ;; esac

# An empty body with rc 0 is not proof of anything either.
stub_reset; STUB_READY_A=true; STUB_READY_B=true; STUB_TIMEOUT=1
STUB_SVC_IP="10.43.0.9"; STUB_WGET_OUT=/dev/null; STUB_WGET_RC=0
run_case service-clusterip
expect "service-clusterip: empty body -> FAIL" 1 "CHECK service-clusterip FAIL" "CHECK service-clusterip PASS"

# --- the same rc audit on every other exec-based check ------------------------
# cross-node-pod-ip: ping printing the success string while exiting non-zero
# must not PASS.
stub_reset; STUB_READY_A=true; STUB_READY_B=true; STUB_TIMEOUT=1
STUB_POD_IP="10.42.1.7"; STUB_PING_OUT="$FIX/ping-ok"; STUB_PING_RC=1
run_case cross-node-pod-ip
expect "cross-node-pod-ip: rc!=0 -> FAIL despite success text" 1 \
    "CHECK cross-node-pod-ip FAIL" "CHECK cross-node-pod-ip PASS"

stub_reset; STUB_READY_A=true; STUB_READY_B=true; STUB_TIMEOUT=1
STUB_POD_IP="10.42.1.7"; STUB_PING_OUT="$FIX/ping-ok"; STUB_PING_RC=0
run_case_on_peer_venue cross-node-pod-ip
expect "cross-node-pod-ip: rc 0 + 0% loss -> PASS" 0 "CHECK cross-node-pod-ip PASS"

stub_reset; STUB_READY_A=true; STUB_READY_B=true; STUB_TIMEOUT=1
STUB_POD_IP="10.42.1.7"; STUB_PING_OUT="$FIX/ping-loss"; STUB_PING_RC=1
run_case cross-node-pod-ip
expect "cross-node-pod-ip: 100% loss -> FAIL" 1 "CHECK cross-node-pod-ip FAIL"

# cluster-dns: the resolver exiting non-zero is not a resolution.
stub_reset; STUB_READY_A=true; STUB_READY_B=true; STUB_TIMEOUT=1
STUB_SVC_IP="10.43.9.9"; STUB_DNS_OUT="$FIX/dns-ok"; STUB_DNS_RC=1
run_case cluster-dns
expect "cluster-dns: rc!=0 -> FAIL despite the address appearing" 1 \
    "CHECK cluster-dns FAIL" "CHECK cluster-dns PASS"

stub_reset; STUB_READY_A=true; STUB_READY_B=true; STUB_TIMEOUT=1
STUB_SVC_IP="10.43.9.9"; STUB_DNS_OUT="$FIX/dns-ok"; STUB_DNS_RC=0
run_case_on_peer_venue cluster-dns
expect "cluster-dns: rc 0 + clusterIP resolved -> PASS" 0 "CHECK cluster-dns PASS"

stub_reset; STUB_READY_A=true; STUB_READY_B=true; STUB_TIMEOUT=1
STUB_SVC_IP="10.43.9.9"; STUB_DNS_OUT="$FIX/dns-nxdomain"; STUB_DNS_RC=1
run_case cluster-dns
expect "cluster-dns: NXDOMAIN -> FAIL" 1 "CHECK cluster-dns FAIL"

# logs-exec: the right strings with a non-zero exec rc is not a working exec.
stub_reset; STUB_READY_A=true; STUB_READY_B=true; STUB_TIMEOUT=1
STUB_LOGS="$FIX/logs-alive"; STUB_EXECO="$FIX/exec-ok"; STUB_EXEC_RC=1
run_case logs-exec
expect "logs-exec: exec rc!=0 -> FAIL despite the marker text" 1 \
    "CHECK logs-exec FAIL" "CHECK logs-exec PASS"

stub_reset; STUB_READY_A=true; STUB_READY_B=true; STUB_TIMEOUT=1
STUB_LOGS="$FIX/logs-alive"; STUB_EXECO="$FIX/exec-ok"
run_case_on_peer_venue logs-exec
expect "logs-exec: rc 0 + both markers -> PASS" 0 "CHECK logs-exec PASS"
# The harness must not claim NODE_B is "the remote/tunnelled node": it never
# verified how the API server reaches that kubelet.
case "$RH_OUT" in *tunnelled*|*"remote node"*) bad "logs-exec must not claim NODE_B is remote/tunnelled" "$RH_OUT" ;; *) ok "logs-exec makes no unverified remote/tunnelled claim" ;; esac

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

# --- headlamp: a backend this harness cannot SEE is missing evidence ----------
# An unlabelled Headlamp deployment means the selector matches nothing, so the
# harness cannot confirm a backend exists. That is a gap in what we can see,
# not an observed defect of the pod network -> BLOCKED, naming the label.
stub_reset; STUB_READY_A=true; STUB_READY_B=true; STUB_TIMEOUT=1
STUB_HL_NS=headlamp; STUB_HL_SVC=headlamp; STUB_HL_PHASE=""
run_case headlamp
expect "headlamp: no pod carries the conventional label -> BLOCKED" 2 \
    "CHECK headlamp BLOCKED" "CHECK headlamp FAIL"
case "$RH_OUT" in *"app.kubernetes.io/name=headlamp"*) ok "headlamp: BLOCKED names the missing label" ;; *) bad "headlamp: BLOCKED names the missing label" "$RH_OUT" ;; esac

# A labelled pod that is not Running is also a precondition, not a defect.
stub_reset; STUB_READY_A=true; STUB_READY_B=true; STUB_TIMEOUT=1
STUB_HL_NS=headlamp; STUB_HL_SVC=headlamp; STUB_HL_PHASE=Pending
run_case headlamp
expect "headlamp: labelled pod not Running -> BLOCKED" 2 "CHECK headlamp BLOCKED" "CHECK headlamp FAIL"

# Running, with a clusterIP, and an HTTP status line comes back: PASS.
stub_reset; STUB_READY_A=true; STUB_READY_B=true; STUB_TIMEOUT=1
STUB_HL_NS=headlamp; STUB_HL_SVC=headlamp; STUB_HL_PHASE=Running
STUB_SVC_IP="10.43.246.3"; STUB_SVC_PORT=80; STUB_WGET_OUT="$FIX/wget-http404"; STUB_WGET_RC=1
run_case_on_peer_venue headlamp
expect "headlamp: reachable Service (404 from / still proves reachability) -> PASS" 0 "CHECK headlamp PASS"

# Running and addressable, but nothing answers: an observed contradiction.
stub_reset; STUB_READY_A=true; STUB_READY_B=true; STUB_TIMEOUT=1
STUB_HL_NS=headlamp; STUB_HL_SVC=headlamp; STUB_HL_PHASE=Running
STUB_SVC_IP="10.43.246.3"; STUB_SVC_PORT=80; STUB_WGET_OUT="$FIX/wget-unreachable"; STUB_WGET_RC=1
run_case headlamp
expect "headlamp: no HTTP status line -> FAIL" 1 "CHECK headlamp FAIL" "CHECK headlamp PASS"

# --- nanochat: bounded rollout (DKS_TIMEOUT), not a fixed sleep --------------
stub_reset; STUB_ROLLOUT_RC=0; STUB_NC_NODES="$FIX/nc-nodes-2"; STUB_NC_READY="$FIX/nc-ready-2"
run_case_on_peer_venue nanochat
expect "nanochat: rollout succeeds, 2 replicas across 2 nodes -> PASS" 0 "CHECK nanochat PASS"

# A slow/failed image pull is a missing precondition, not a placement defect:
# it must BLOCK, never FAIL, and must name the precondition.
stub_reset; STUB_ROLLOUT_RC=1; STUB_NC_PULL="$FIX/pull-issue"
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

# --- bashy-chunked: the same ImagePullBackOff carve-out ----------------------
# Without it, an image that never became pullable was scored as a distributed
# workload that ran and came out wrong.
stub_reset; STUB_BASHY_IMG="stub/bashy:latest"; STUB_BJ_PULL="$FIX/pull-issue"
STUB_TIMEOUT=1
run_case bashy-chunked
expect "bashy-chunked: image never pullable -> BLOCKED" 2 \
    "CHECK bashy-chunked BLOCKED" "CHECK bashy-chunked FAIL"
case "$RH_OUT" in *"not pullable within"*) ok "bashy-chunked: pull-issue reason named" ;; *) bad "bashy-chunked: pull-issue reason named" "$RH_OUT" ;; esac

# A genuine distributed-workload failure (no pull issue) must still FAIL.
stub_reset; STUB_BASHY_IMG="stub/bashy:latest"; STUB_BJ_SUCC=2
STUB_BJ_NODES="$FIX/nc-nodes-1"; STUB_TIMEOUT=1
run_case bashy-chunked
expect "bashy-chunked: incomplete job, no pull issue -> FAIL" 1 \
    "CHECK bashy-chunked FAIL" "CHECK bashy-chunked BLOCKED"

stub_reset; STUB_BASHY_IMG="stub/bashy:latest"; STUB_BJ_SUCC=4
STUB_BJ_NODES="$FIX/nc-nodes-2"; STUB_TIMEOUT=1
run_case_on_peer_venue bashy-chunked
expect "bashy-chunked: 4/4 across 2 nodes -> PASS" 0 "CHECK bashy-chunked PASS"

stub_reset; STUB_BASHY_IMG=""
run_case bashy-chunked
expect "bashy-chunked: DKS_BASHY_IMAGE unset -> BLOCKED" 2 "CHECK bashy-chunked BLOCKED"

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
# The negative blacklist must not come back: "PASS unless the output mentions
# one of these words" is how a broken network scored green.
if grep -qE "grep -qiE 'refused\|timed out" "$HARNESS"; then
    bad "harness must not score a check on a negative error blacklist"
else
    ok "harness scores no check on a negative error blacklist"
fi
# Every check name in the story must be implemented.
for n in nodes-ready peer-venue distinct-pod-cidrs flannel-iface no-stale-conflist \
         cross-node-pod-ip service-clusterip cluster-dns logs-exec \
         headlamp nanochat bashy-chunked; do
    if grep -q "dks_selected $n" "$HARNESS"; then ok "check implemented: $n"; else bad "check missing: $n"; fi
done

echo
echo "TESTS pass=$T_PASS fail=$T_FAIL"
[ "$T_FAIL" -eq 0 ]
