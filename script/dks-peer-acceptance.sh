#!/usr/bin/env bash
# dks-peer-acceptance.sh — acceptance runner for peer-hosted DKS multi-node
# pod networking (ADR: Tailscale node underlay + stock k3s flannel VXLAN
# pinned via --flannel-iface=tailscale0; see docs/adr-peer-dks-pod-network.md).
#
# Emits one machine-readable line per check:
#     CHECK <name> PASS|FAIL|BLOCKED|ATTESTED <detail>
# and a final summary.
#
# ---------------------------------------------------------------------------
# The four statuses
# ---------------------------------------------------------------------------
#   PASS      the check's claim was OBSERVED to hold, by this harness, on this
#             cluster. Only PASS is evidence.
#   FAIL      an observed contradiction of the claim.
#   BLOCKED   a precondition was absent, so the claim was never tested. NOT a
#             pass and NOT a failure. A check is never silently skipped.
#   ATTESTED  the claim would hold, but at least one load-bearing item came
#             from an OPERATOR-SUPPLIED evidence file (DKS_HOST_EVIDENCE)
#             rather than from a machine this harness inspected. Operator
#             attestation is NOT observation: ATTESTED never counts toward a
#             green verdict and can never be rendered as PASS. See
#             "Evidence provenance" below.
#
# ---------------------------------------------------------------------------
# Precondition checks vs SUBSTANTIVE (evidence) checks
# ---------------------------------------------------------------------------
# THE CLASSIFICATION RULE, and it is a test you apply to every new check:
#
#     If the cluster were completely broken — no CNI, no routes, not one packet
#     able to cross between nodes — but the API SERVER were healthy, could this
#     check still PASS?
#
#     YES  -> it is a PRECONDITION. It describes the venue, not the slice.
#     NO   -> it is SUBSTANTIVE, and only because it MOVED A PACKET or
#             INSPECTED A HOST.
#
# A substantive check must rest on something the API server cannot fabricate:
# bytes that crossed the pod network, a workload the kubelets actually ran, or
# host state read through an inspection pod. Reading a FIELD out of the API
# server is never evidence about the slice, however specific the field looks —
# an object's spec is bookkeeping written at registration time, and it stays
# true while the dataplane underneath it is dead.
#
# `distinct-pod-cidrs` is the worked example, and it is classified as a
# PRECONDITION for exactly this reason: it reads `.spec.podCIDR` and nothing
# else. Distinct per-node podCIDRs are IPAM bookkeeping that k3s assigns at
# node registration under its default `--allocate-node-cidrs`. They are true
# whether or not flannel is running, whether or not `tailscale0` exists, and
# whether or not a single byte can cross between nodes. It sends no probe pod,
# inspects no host, and moves no packet. It answers YES to the rule above, so
# it cannot carry a verdict — even though it was Substantive for five sprints.
#
# The classification is enforced POSITIVELY, by allowlist (DKS_EVIDENCE_CHECKS
# below), never by blacklist: a check that is not explicitly listed as evidence
# contributes NOTHING to a green verdict. That way a future API-only check
# cannot inherit the saturating role by being forgotten — the default is
# "not evidence", and making a check count requires stating that it moves a
# packet or inspects a host.
#
# A run in which no SUBSTANTIVE check passed proves nothing, so it is
# INCONCLUSIVE (exit 2) even if every precondition passed. A single trivially
# true precondition must never be able to saturate the "something passed"
# requirement — that is exactly the absence-of-evidence-as-success failure
# docs/fleet-evidence-invariant.md forbids.
#
# ---------------------------------------------------------------------------
# The venue assertion is BINDING
# ---------------------------------------------------------------------------
# `peer-venue` is a precondition, but a LOAD-BEARING one: it is the only check
# that asserts this is the peer-hosted DKS plane at all, which is the entire
# point of a *peer*-DKS gate. So it binds the verdict:
#
#     peer-venue PASS  ..... required for any green verdict.
#     anything else — FAIL, BLOCKED, or NOT SELECTED — caps the verdict at
#     INCONCLUSIVE (or FAIL, where something already failed).
#
# In particular it is not selectable-away: DKS_ONLY omitting `peer-venue` can
# no longer produce exit 0. Before this rule, the same cluster exited 1 with
# the venue check selected ("this is NOT a peer-hosted DKS plane") and 0
# without it — a cluster the harness ITSELF declared non-peer passed the gate.
# A BLOCKED venue caps too: "we could not tell where we are" is not a licence
# to certify what we found there.
#
# ---------------------------------------------------------------------------
# Exit status (the contract a CI gate reads; the exit code carries the full
# verdict, and every code below is asserted against the real runner process by
# script/dks-peer-acceptance_test.sh)
# ---------------------------------------------------------------------------
#     0  OK           — BOTH of: `peer-venue` PASSed (the venue was positively
#                       asserted), AND at least one SUBSTANTIVE check PASSed
#                       (observed); and nothing FAILed.
#     1  FAIL         — at least one check FAILed. Outranks INCONCLUSIVE.
#     2  INCONCLUSIVE — nothing was proven. Covers "no check ran", "all
#                       BLOCKED", "only preconditions passed", "only
#                       operator-attested evidence", and "the peer venue was
#                       never asserted (peer-venue BLOCKED or not selected)".
#                       Absence of evidence is NOT success.
#
# A gate may read the exit status alone: it carries the full verdict, including
# the venue assertion. Exit 0 means this harness observed the peer venue AND
# observed at least one property of the slice under test — neither can be
# skipped, deselected, or satisfied by an API-server read.
#
# ---------------------------------------------------------------------------
# Evidence provenance
# ---------------------------------------------------------------------------
# Two kinds of evidence reach this harness and they are kept structurally
# distinct, in the CHECK line and in the summary:
#
#   OBSERVED  — read by this harness from the cluster/API server, or from a
#               host-inspection pod it created and whose logs it collected.
#   ATTESTED  — read out of the operator-authored DKS_HOST_EVIDENCE file. No
#               machine was inspected; a human typed it.
#
# DECISION: operator-attested evidence may NOT contribute to a green verdict.
# It is accepted and reported, because an operator's collected evidence is
# genuinely useful for triage on a host where inspection RBAC is unavailable,
# and because reading it is how a contradiction gets surfaced at all. But a
# check whose only path to passing runs through attested items records
# ATTESTED, not PASS, and the run exits 2 unless something else was actually
# observed. Every evidence item is tagged (observed) or (attested) in the
# detail text, so an ATTESTED record can never be re-presented as an observed
# one.
#
# Contains NO secrets and never prints kubeconfig/token material.
#
# Usage:
#   KUBECONFIG=/path/to/kubeconfig ./script/dks-peer-acceptance.sh
#   DKS_ONLY=nodes-ready,distinct-pod-cidrs ./script/dks-peer-acceptance.sh
#
# Env:
#   KUBECONFIG            kubeconfig path (else kubectl default)
#   DKS_NAMESPACE         namespace for test workloads (default: current context ns, else "default")
#   DKS_NODE_A/DKS_NODE_B pin the two nodes under test (default: auto-pick two Ready k3s-agent nodes)
#   DKS_ONLY              comma-separated check names to run (default: all)
#   DKS_TIMEOUT           seconds to wait for a pod to become Ready (default 120)
#   DKS_TEST_IMAGE        image for the probe pods (default docker.io/library/busybox:1.36)
#   DKS_KEEP=1            do not delete created test objects
#   DKS_ALLOW_NODE_DEBUG=1  permit host-inspection pods (hostPID + hostNetwork +
#                         read-only hostPath mount of / at /host) for the
#                         flannel-iface and no-stale-conflist host-evidence checks
#   DKS_HOST_EVIDENCE     file of OPERATOR-ATTESTED host evidence, consulted for
#                         any item the inspection pods did not produce (or when
#                         inspection is not permitted). One node per line:
#                             <node> <key>=<value> [<key>=<value> ...]
#                         with the same vocabulary the inspection pods emit (see
#                         dks_ev). Without pod evidence or this file the
#                         host-evidence checks stay BLOCKED — a missing tool or
#                         missing evidence is never scored FAIL; only observed
#                         contradictory values are. Attested items can only
#                         produce ATTESTED, never PASS.
#   DKS_HEADLAMP_NS/DKS_HEADLAMP_SVC   an already-deployed Headlamp to assert against
#   DKS_NANOCHAT_IMAGE    image for the nanochat cross-node workload
#   DKS_BASHY_IMAGE       image for the chunked bashy distributed workload
#
# Sourcing this file with DKS_LIB_ONLY=1 loads the pure helper functions
# (record/tally/parse/compare) without running any check — this is what the
# offline tests in script/dks-peer-acceptance_test.sh exercise.

set -uo pipefail

# ---------------------------------------------------------------------------
# Pure helpers (offline-testable; no kubectl, no network)
# ---------------------------------------------------------------------------

DKS_PASS_COUNT=0
DKS_FAIL_COUNT=0
DKS_BLOCKED_COUNT=0
DKS_ATTESTED_COUNT=0
# Passes that actually prove something about the slice under test — i.e. a PASS
# from a check on the DKS_EVIDENCE_CHECKS allowlist. Preconditions and any
# unclassified check are excluded, so neither can saturate the "at least one
# PASS" requirement.
DKS_SUBSTANTIVE_PASS_COUNT=0
# Outcome of the binding venue assertion: "" until peer-venue records anything,
# then its literal status. Set from dks_record itself so it can never disagree
# with the CHECK line that was actually emitted.
DKS_VENUE_STATUS=""
DKS_RESULTS=()

# Checks that establish the venue rather than test the slice. Passing one of
# these is necessary, never sufficient.
#
# `distinct-pod-cidrs` sits here — not with the evidence checks — because it
# reads .spec.podCIDR from the API server and nothing else; see "The
# classification rule" in the header. It was the successor saturator: the one
# substantive check that survived a degraded environment, so it was precisely
# the check that still passed when everything else blocked.
DKS_PRECONDITION_CHECKS="preflight nodes-ready peer-venue distinct-pod-cidrs"

# THE evidence allowlist. Only a PASS from a check named here can make a run
# green. Each one either MOVED A PACKET across the pod network or INSPECTED A
# HOST; none can pass on an API-server read alone:
#
#   flannel-iface       host k3s argv + host tailscale0, via inspection pods
#   no-stale-conflist   host /etc/cni/net.d, via inspection pods
#   cross-node-pod-ip   ICMP pod->pod across nodes
#   service-clusterip   HTTP pod->clusterIP->backend pod on the other node
#   cluster-dns         DNS query answered by the in-cluster resolver
#   logs-exec           kubelet log/exec stream against a pod on the far node
#   headlamp            HTTP pod->Service, cross-node
#   nanochat            a real workload the kubelets pulled and RAN on 2 nodes
#   bashy-chunked       indexed Job chunks actually completed across 2 nodes
#
# ALLOWLIST, DELIBERATELY, NOT A BLACKLIST: an unlisted check contributes
# nothing to a green verdict. Adding a check here is an explicit claim that it
# moves a packet or inspects a host — so a future API-only check cannot become
# the next saturator by default.
DKS_EVIDENCE_CHECKS="flannel-iface no-stale-conflist cross-node-pod-ip service-clusterip cluster-dns logs-exec headlamp nanochat bashy-chunked"

# dks_is_precondition <name> — 0 when <name> is a precondition check.
dks_is_precondition() {
    case " $DKS_PRECONDITION_CHECKS " in
        *" ${1:-} "*) return 0 ;;
    esac
    return 1
}

# dks_is_evidence <name> — 0 when a PASS from <name> may count toward a green
# verdict. Unlisted => not evidence (the safe default; see the allowlist note).
dks_is_evidence() {
    case " $DKS_EVIDENCE_CHECKS " in
        *" ${1:-} "*) return 0 ;;
    esac
    return 1
}

# dks_record <name> <PASS|FAIL|BLOCKED|ATTESTED> <detail...>
dks_record() {
    local name="$1" status="$2"
    shift 2
    local detail="$*"
    # Collapse newlines AND carriage returns so one check is always exactly one
    # line. \r matters: wget -S / nslookup emit CRLF, and a bare \r makes a
    # terminal overwrite the line, silently hiding a result.
    detail="${detail//$'\r'/ }"
    detail="${detail//$'\n'/ | }"
    case "$status" in
        PASS)
            DKS_PASS_COUNT=$((DKS_PASS_COUNT + 1))
            # Allowlist, not "not a precondition": a check nobody classified
            # must not become evidence by omission.
            dks_is_evidence "$name" && DKS_SUBSTANTIVE_PASS_COUNT=$((DKS_SUBSTANTIVE_PASS_COUNT + 1))
            ;;
        FAIL)     DKS_FAIL_COUNT=$((DKS_FAIL_COUNT + 1)) ;;
        BLOCKED)  DKS_BLOCKED_COUNT=$((DKS_BLOCKED_COUNT + 1)) ;;
        # Operator-attested. Deliberately tallied on its OWN counter: it must
        # not reach DKS_PASS_COUNT (which would make it green) nor
        # DKS_FAIL_COUNT (which would make it red). It is neither.
        ATTESTED) DKS_ATTESTED_COUNT=$((DKS_ATTESTED_COUNT + 1)) ;;
        *)
            # An unknown status is itself a harness defect; never let it pass.
            DKS_FAIL_COUNT=$((DKS_FAIL_COUNT + 1))
            status="FAIL"
            detail="harness error: unknown status; $detail"
            ;;
    esac
    # The binding venue assertion. Recorded here, from the status actually
    # emitted, so the verdict cap can never drift from the CHECK line.
    [ "$name" = "peer-venue" ] && DKS_VENUE_STATUS="$status"
    DKS_RESULTS+=("CHECK $name $status $detail")
    echo "CHECK $name $status $detail"
}

# dks_summary — prints the tally; returns the harness exit code:
#   0 OK (peer-venue PASSed AND >=1 SUBSTANTIVE observed PASS, 0 FAIL)
#   1 FAIL (>=1 FAIL)
#   2 INCONCLUSIVE (the venue was not asserted, or nothing substantive was
#     observed to pass).
dks_summary() {
    echo "SUMMARY pass=$DKS_PASS_COUNT fail=$DKS_FAIL_COUNT blocked=$DKS_BLOCKED_COUNT attested=$DKS_ATTESTED_COUNT substantive_pass=$DKS_SUBSTANTIVE_PASS_COUNT venue=${DKS_VENUE_STATUS:-NOT-RUN}"
    if [ "$DKS_FAIL_COUNT" -gt 0 ]; then
        echo "RESULT FAIL"
        return 1
    fi

    # The BINDING venue assertion. Anything other than an observed PASS caps
    # the verdict: without it we do not know we are on the peer-hosted DKS
    # plane, so every other result in this run is about some unidentified
    # venue. Deliberately NOT skippable via DKS_ONLY.
    local venue_note=""
    case "$DKS_VENUE_STATUS" in
        PASS) ;;
        "")   venue_note="; the peer venue was never asserted (peer-venue did not run; DKS_ONLY cannot deselect it out of the verdict)" ;;
        BLOCKED) venue_note="; the peer venue could not be asserted (peer-venue BLOCKED) — 'we could not tell where we are' is not a licence to certify what we found there" ;;
        *)    venue_note="; the peer venue was not asserted (peer-venue $DKS_VENUE_STATUS)" ;;
    esac

    # Name WHICH flavour of nothing, because "all blocked", "only the
    # preconditions passed" and "only an operator said so" need different
    # operator responses.
    local base=""
    if [ "$DKS_SUBSTANTIVE_PASS_COUNT" -eq 0 ]; then
        local total=$((DKS_PASS_COUNT + DKS_FAIL_COUNT + DKS_BLOCKED_COUNT + DKS_ATTESTED_COUNT))
        if [ "$total" -eq 0 ]; then
            base="no check ran"
        elif [ "$DKS_ATTESTED_COUNT" -gt 0 ]; then
            base="only operator-attested evidence; no substantive check was observed to pass"
        elif [ "$DKS_PASS_COUNT" -gt 0 ]; then
            base="only precondition checks passed; no substantive check proved anything"
        else
            base="no check passed"
        fi
    else
        base="$DKS_SUBSTANTIVE_PASS_COUNT substantive PASS observed"
    fi

    # Nothing about the slice was proven, OR we do not know where we are.
    # Either way exit NON-ZERO (2) — a gate that reads only the exit code must
    # never score "no evidence" as a pass.
    if [ "$DKS_SUBSTANTIVE_PASS_COUNT" -eq 0 ] || [ -n "$venue_note" ]; then
        echo "RESULT INCONCLUSIVE ($base$venue_note)"
        return 2
    fi
    echo "RESULT OK ($DKS_SUBSTANTIVE_PASS_COUNT substantive PASS observed on a peer venue asserted by peer-venue; no failures; $DKS_BLOCKED_COUNT blocked; $DKS_ATTESTED_COUNT operator-attested and NOT counted as proof)"
    return 0
}

# dks_distinct_cidrs — reads "<node> <podCIDR>" lines on stdin.
# Prints a reason and returns 1 when any CIDR is empty or duplicated.
dks_distinct_cidrs() {
    local seen="" node cidr dup="" empty="" n=0
    while read -r node cidr _rest; do
        [ -z "${node:-}" ] && continue
        n=$((n + 1))
        if [ -z "${cidr:-}" ] || [ "$cidr" = "<none>" ] || [ "$cidr" = "null" ]; then
            empty="$empty $node"
            continue
        fi
        case " $seen " in
            *" $cidr "*) dup="$dup $node=$cidr" ;;
            *) seen="$seen $cidr" ;;
        esac
    done
    if [ "$n" -eq 0 ]; then
        echo "no nodes supplied"
        return 1
    fi
    if [ -n "$empty" ]; then
        echo "empty podCIDR on:$empty"
        return 1
    fi
    if [ -n "$dup" ]; then
        echo "duplicate podCIDR on:$dup"
        return 1
    fi
    echo "$n distinct podCIDRs:$seen"
    return 0
}

# dks_is_tailnet_ip — true for a Tailscale CGNAT address (100.64.0.0/10).
dks_is_tailnet_ip() {
    local ip="${1:-}"
    case "$ip" in
        100.*)
            local second="${ip#100.}"
            second="${second%%.*}"
            case "$second" in
                ''|*[!0-9]*) return 1 ;;
            esac
            [ "$second" -ge 64 ] && [ "$second" -le 127 ]
            return $?
            ;;
    esac
    return 1
}

# dks_selected — honours DKS_ONLY. Returns 0 when <name> should run.
dks_selected() {
    local name="$1"
    [ -z "${DKS_ONLY:-}" ] && return 0
    case ",${DKS_ONLY}," in
        *",$name,"*) return 0 ;;
    esac
    return 1
}

# dks_ev <blob> <key> — value of the first "EV <key>=<value>" line in <blob>.
# The inspection pods and the DKS_HOST_EVIDENCE file share this one vocabulary:
#   k3s_argv    = flannel-iface-tailscale0 | no-flannel-iface | absent
#   tailscale0  = ipv4:<addr> | no-ipv4 | absent | tool-missing
#   cni_confdir = listing:<comma-list> | empty | dir-absent | unreadable
#                 | host-mount-missing
# "absent"/"tool-missing"/"unreadable"/"host-mount-missing" (and a missing key)
# are MISSING evidence -> BLOCKED; "no-flannel-iface"/"no-ipv4"/a stale listing
# are OBSERVED contradictions -> eligible for FAIL.
dks_ev() {
    printf '%s\n' "${1:-}" | sed -n "s/^EV ${2}=//p" | head -1
}

# dks_file_ev <node> <key> — OPERATOR-ATTESTED host evidence from the file named
# by DKS_HOST_EVIDENCE (format documented in the header). Values are space-free.
# Prints nothing when the file, node, or key is absent — callers treat that as
# missing evidence (BLOCKED), never FAIL.
dks_file_ev() {
    [ -n "${DKS_HOST_EVIDENCE:-}" ] && [ -r "${DKS_HOST_EVIDENCE}" ] || return 1
    awk -v node="$1" -v key="$2" '
        $1 == node {
            for (i = 2; i <= NF; i++)
                if (index($i, key "=") == 1) { print substr($i, length(key) + 2); exit }
        }' "$DKS_HOST_EVIDENCE"
}

# dks_resolve_ev <inspection-blob> <node> <key>
# Resolves one evidence item and RECORDS ITS PROVENANCE. Sets two globals:
#   DKS_EV_VALUE   the value ("" when nothing is known)
#   DKS_EV_SOURCE  observed | attested | none
# Observed evidence (inspection-pod logs) always wins over attested evidence;
# the caller passes an empty blob when inspection was not permitted or produced
# nothing. Returns 0 when a value was found. Globals, not stdout: the callers
# run this in the main shell and a $( ) subshell would lose the provenance.
DKS_EV_VALUE=""
DKS_EV_SOURCE="none"
dks_resolve_ev() {
    DKS_EV_VALUE="$(dks_ev "${1:-}" "$3")"
    if [ -n "$DKS_EV_VALUE" ]; then
        DKS_EV_SOURCE="observed"
        return 0
    fi
    DKS_EV_VALUE="$(dks_file_ev "$2" "$3" 2>/dev/null)" || DKS_EV_VALUE=""
    if [ -n "$DKS_EV_VALUE" ]; then
        DKS_EV_SOURCE="attested"
        return 0
    fi
    DKS_EV_SOURCE="none"
    return 1
}

# dks_run <cmd> [args...] — run a command, capturing its stdout, its stderr and
# its RETURN CODE into globals. Returns that return code.
#
# This exists because of a whole class of defect in this harness: a query
# written as
#     OUT="$(kubectl get ... 2>/dev/null)"
# discards both the error text and the return code, so an RBAC denial, an
# expired credential or a transient API error is indistinguishable from a
# healthy cluster that legitimately has nothing to report. The caller then sees
# an empty string and reports "flannel is not running" or "no node returned a
# podCIDR" — a confident, WRONG diagnosis of a query that never ran.
#
# THE RULE: a check that COULD NOT RUN must be distinguishable from a check
# that RAN AND FOUND NOTHING. The first is BLOCKED with the transport error
# named; only the second may be scored against the claim.
DKS_RUN_OUT=""; DKS_RUN_ERR=""; DKS_RUN_RC=0
dks_run() {
    local errf
    errf="$(mktemp 2>/dev/null)" || errf=""
    if [ -z "$errf" ]; then
        # No temp file available: still capture rc, and say that stderr was
        # not captured rather than implying the command produced none.
        DKS_RUN_OUT="$("$@" 2>/dev/null)"
        DKS_RUN_RC=$?
        DKS_RUN_ERR="(stderr not captured: mktemp unavailable)"
        return "$DKS_RUN_RC"
    fi
    DKS_RUN_OUT="$("$@" 2>"$errf")"
    DKS_RUN_RC=$?
    # Single line, bounded: this text lands in a CHECK detail line.
    DKS_RUN_ERR="$(tr '\n' ' ' <"$errf" | head -c 200)"
    rm -f "$errf"
    return "$DKS_RUN_RC"
}

# Guard: sourcing for tests stops here.
if [ "${DKS_LIB_ONLY:-0}" = "1" ]; then
    return 0 2>/dev/null || true
fi

# ---------------------------------------------------------------------------
# Live-cluster plumbing
# ---------------------------------------------------------------------------

DKS_TIMEOUT="${DKS_TIMEOUT:-120}"
DKS_TEST_IMAGE="${DKS_TEST_IMAGE:-docker.io/library/busybox:1.36}"
RUN_ID="dksacc-$$"
CREATED=()

k() { kubectl ${DKS_NAMESPACE:+-n "$DKS_NAMESPACE"} "$@"; }

cleanup() {
    [ "${DKS_KEEP:-0}" = "1" ] && { echo "NOTE keeping test objects (DKS_KEEP=1): ${CREATED[*]:-none}"; return; }
    local obj
    for obj in "${CREATED[@]:-}"; do
        [ -n "$obj" ] && k delete "$obj" --ignore-not-found --wait=false >/dev/null 2>&1
    done
}
trap cleanup EXIT

# Wait for a pod to be Ready; echoes "" on success, the reason on failure.
wait_ready() {
    local pod="$1" deadline=$((SECONDS + DKS_TIMEOUT)) phase=""
    while [ "$SECONDS" -lt "$deadline" ]; do
        phase="$(k get pod "$pod" -o jsonpath='{.status.phase}' 2>/dev/null)"
        if [ "$(k get pod "$pod" -o jsonpath='{.status.containerStatuses[0].ready}' 2>/dev/null)" = "true" ]; then
            echo ""; return 0
        fi
        [ "$phase" = "Failed" ] && break
        sleep 3
    done
    echo "pod/$pod not Ready within ${DKS_TIMEOUT}s (phase=${phase:-unknown}): $(k get pod "$pod" -o jsonpath='{.status.conditions[?(@.type=="PodScheduled")].message}' 2>/dev/null | head -c 200)"
    return 1
}

make_pod() {
    local name="$1" node="$2"
    # Register for cleanup BEFORE creation: a create that half-succeeds, or a
    # harness killed mid-apply, must still delete the pod on the EXIT trap.
    CREATED+=("pod/$name")
    cat <<YAML | k apply -f - >/dev/null 2>&1
apiVersion: v1
kind: Pod
metadata:
  name: $name
  labels: {app: $name, harness: "$RUN_ID"}
spec:
  nodeName: $node
  restartPolicy: Never
  terminationGracePeriodSeconds: 1
  containers:
  - name: probe
    image: $DKS_TEST_IMAGE
    command: ["sh","-c","while true; do echo dks-alive; sleep 5; done"]
YAML
}

# ---------------------------------------------------------------------------
# Host-inspection pods
# ---------------------------------------------------------------------------
# The pod-side scripts print ONLY "EV key=value" lines (vocabulary at dks_ev).
# They run with hostPID (host /proc visible), hostNetwork (host interfaces
# visible), and / mounted read-only at /host — an ordinary pod sees none of
# host k3s argv, host tailscale0, or /etc/cni/net.d, so its output would be
# invalid evidence. A missing tool or invisible process is reported as its own
# value (absent / tool-missing), never conflated with a contradiction.
#
# DKSMARK: with hostPID the /proc scan also sees this script's own sh
# processes, whose cmdline contains these literal match patterns; every
# process carrying the marker is skipped so the scan can never match itself.
FLANNEL_INSPECT_SCRIPT='argv=""
for f in /proc/[0-9]*/cmdline; do
    c="$(tr "\000" " " < "$f" 2>/dev/null)" || continue
    case "$c" in *DKSMARK*) continue ;; esac
    case "$c" in *"k3s agent "*) argv="$c"; break ;; esac
done
if [ -z "$argv" ]; then
    echo "EV k3s_argv=absent"
else
    case "$argv" in
        *"--flannel-iface=tailscale0 "*) echo "EV k3s_argv=flannel-iface-tailscale0" ;;
        *) echo "EV k3s_argv=no-flannel-iface" ;;
    esac
fi
ipbin=""
if command -v ip >/dev/null 2>&1; then
    ipbin="ip"
elif chroot /host ip -V >/dev/null 2>&1; then
    ipbin="chroot /host ip"
fi
if [ -z "$ipbin" ]; then
    echo "EV tailscale0=tool-missing"
elif ! $ipbin link show dev tailscale0 >/dev/null 2>&1; then
    echo "EV tailscale0=absent"
else
    a="$($ipbin -4 -o addr show dev tailscale0 2>/dev/null | sed -n "s/.*inet \([0-9.]*\).*/\1/p" | head -1)"
    if [ -n "$a" ]; then echo "EV tailscale0=ipv4:$a"; else echo "EV tailscale0=no-ipv4"; fi
fi'

CONFLIST_INSPECT_SCRIPT='if [ ! -d /host/etc ]; then
    echo "EV cni_confdir=host-mount-missing"
elif [ ! -e /host/etc/cni/net.d ]; then
    echo "EV cni_confdir=dir-absent"
elif files="$(ls /host/etc/cni/net.d 2>/dev/null)"; then
    if [ -z "$files" ]; then
        echo "EV cni_confdir=empty"
    else
        echo "EV cni_confdir=listing:$(printf "%s" "$files" | tr "\n" ",")"
    fi
else
    echo "EV cni_confdir=unreadable"
fi'

# dks_inspect <slug> <idx> <node> <script>
# One-shot host-inspection pod pinned to <node>. The name is deterministic,
# DNS-safe, and INDEXED — ${RUN_ID}-insp-<slug>-<idx> — never derived from the
# node name (a prefixed node name can exceed or violate the RFC 1123 label
# rules). Registers the pod for cleanup BEFORE kubectl is asked to create it,
# waits (bounded by DKS_TIMEOUT) for it to run to completion, fetches logs,
# and deletes it explicitly — no best-effort `--rm` attach semantics.
#
# Logs land in $DKS_INSPECT_OUT, a global, NOT on stdout: running this inside
# $(...) would fork a subshell and silently lose the CREATED registration.
# Returns non-zero when no logs were captured (caller treats as no evidence).
DKS_INSPECT_OUT=""
dks_inspect() {
    local slug="$1" idx="$2" node="$3" script="$4"
    local name="${RUN_ID}-insp-${slug}-${idx}"
    DKS_INSPECT_OUT=""
    CREATED+=("pod/$name")
    if ! cat <<YAML | k apply -f - >/dev/null 2>&1
apiVersion: v1
kind: Pod
metadata:
  name: $name
  labels: {harness: "$RUN_ID"}
spec:
  nodeName: $node
  restartPolicy: Never
  terminationGracePeriodSeconds: 1
  hostPID: true
  hostNetwork: true
  containers:
  - name: inspect
    image: $DKS_TEST_IMAGE
    command: ["sh","-c"]
    args:
    - |
$(printf '%s\n' "$script" | sed 's/^/      /')
    volumeMounts:
    - {name: host, mountPath: /host, readOnly: true}
  volumes:
  - name: host
    hostPath: {path: /, type: Directory}
YAML
    then
        k delete pod "$name" --ignore-not-found --wait=false >/dev/null 2>&1
        return 1
    fi
    local deadline=$((SECONDS + DKS_TIMEOUT)) phase=""
    while [ "$SECONDS" -lt "$deadline" ]; do
        phase="$(k get pod "$name" -o jsonpath='{.status.phase}' 2>/dev/null)"
        case "$phase" in Succeeded|Failed) break ;; esac
        sleep 2
    done
    case "$phase" in
        Succeeded|Failed) DKS_INSPECT_OUT="$(k logs "$name" 2>/dev/null)" ;;
    esac
    k delete pod "$name" --ignore-not-found --wait=false >/dev/null 2>&1
    [ -n "$DKS_INSPECT_OUT" ]
}

# ---------------------------------------------------------------------------
# Preflight
# ---------------------------------------------------------------------------

if ! command -v kubectl >/dev/null 2>&1; then
    dks_record preflight BLOCKED "kubectl not on PATH"
    dks_summary; exit $?
fi

NODES_RAW="$(kubectl get nodes -o json 2>&1)"
if [ $? -ne 0 ]; then
    dks_record preflight BLOCKED "cannot reach cluster: $(echo "$NODES_RAW" | head -c 200)"
    dks_summary; exit $?
fi

if [ -z "${DKS_NAMESPACE:-}" ]; then
    DKS_NAMESPACE="$(kubectl config view --minify -o jsonpath='{..namespace}' 2>/dev/null)"
    DKS_NAMESPACE="${DKS_NAMESPACE:-default}"
fi
echo "NOTE namespace=$DKS_NAMESPACE run_id=$RUN_ID image=$DKS_TEST_IMAGE"

# Real (kubelet-backed) nodes only. virtual-kubelet nodes report a vknode
# kubelet version and do not participate in pod networking at all, so
# including them would make cross-node pod IP checks meaningless.
# mapfile/readarray is not available in every bash-compatible runner, and a
# nested ${arr[0]:-} default trips `set -u` in some of them; keep it to a
# whitespace-separated string built by a plain pipeline.
#
# The rc is CHECKED (dks_run), not discarded. This query previously swallowed
# both stderr and rc, so an RBAC denial produced an empty READY_LIST that was
# then reported as "found 0 Ready nodes" — a wrong diagnosis that additionally
# BLOCKed every downstream check while `distinct-pod-cidrs` sailed on past it
# off its own separate query, and the run still exited 0.
NODE_QUERY_OK=1; NODE_QUERY_ERR=""; NODE_QUERY_RC=0
if dks_run kubectl get nodes -o \
    'jsonpath={range .items[*]}{.metadata.name}{" "}{.status.nodeInfo.kubeletVersion}{" "}{range .status.conditions[?(@.type=="Ready")]}{.status}{end}{"\n"}{end}'
then
    READY_LIST="$(printf '%s\n' "$DKS_RUN_OUT" \
        | awk '$3=="True" && $2 !~ /vknode/ && $2 ~ /k3s|^v1\./ {print $1}' | tr '\n' ' ')"
else
    NODE_QUERY_OK=0; NODE_QUERY_RC="$DKS_RUN_RC"; NODE_QUERY_ERR="$DKS_RUN_ERR"
    READY_LIST=""
    echo "NOTE node-list query FAILED rc=$NODE_QUERY_RC: ${NODE_QUERY_ERR:-no stderr}"
fi
READY_COUNT="$(echo "$READY_LIST" | wc -w | tr -d ' ')"

NODE_A="${DKS_NODE_A:-}"; NODE_B="${DKS_NODE_B:-}"
[ -z "$NODE_A" ] && NODE_A="$(echo "$READY_LIST" | awk '{print $1}')"
[ -z "$NODE_B" ] && NODE_B="$(echo "$READY_LIST" | awk '{print $2}')"
echo "NOTE ready_real_nodes=$READY_COUNT node_a=${NODE_A:-none} node_b=${NODE_B:-none}"
echo "NOTE ready_real_node_list=$READY_LIST"

TWO_NODES=0
[ -n "$NODE_A" ] && [ -n "$NODE_B" ] && [ "$NODE_A" != "$NODE_B" ] && TWO_NODES=1
# "could not run" and "ran and found nothing" get DIFFERENT words.
if [ "$NODE_QUERY_OK" = "1" ]; then
    NO_TWO="requires two Ready kubelet-backed nodes in one cluster; found $READY_COUNT"
else
    NO_TWO="the node-list query COULD NOT RUN (rc=$NODE_QUERY_RC: ${NODE_QUERY_ERR:-no stderr}); the venue was never enumerated, which is not the same as finding 0 nodes"
fi

# ---------------------------------------------------------------------------
# 1. nodes-ready  (PRECONDITION — never evidence about the slice under test)
# A venue with fewer than two Ready kubelet-backed nodes is a MISSING
# PRECONDITION, not a defect of the code under test: nothing was tested, so
# BLOCKED (-> INCONCLUSIVE), never FAIL. Scoring it FAIL would invert the
# harness's own doctrine and would also mask the real reason the run is
# worthless (there was no venue).
# ---------------------------------------------------------------------------
if dks_selected nodes-ready; then
    if [ "$NODE_QUERY_OK" != "1" ]; then
        dks_record nodes-ready BLOCKED "the node-list query COULD NOT RUN (rc=$NODE_QUERY_RC): ${NODE_QUERY_ERR:-no stderr}; the venue was never enumerated — this is NOT 'zero Ready nodes'"
    elif [ "$READY_COUNT" -ge 2 ]; then
        dks_record nodes-ready PASS "$READY_COUNT Ready kubelet-backed nodes: $READY_LIST"
    else
        dks_record nodes-ready BLOCKED "$NO_TWO (${READY_LIST:-none}); a one-node venue proves nothing either way"
    fi
fi

# ---------------------------------------------------------------------------
# 2. peer-venue  (PRECONDITION — asserts WHERE we are)
# THE venue assertion. Without it this harness will happily emit a green
# verdict against ANY two-node cluster, including one that has nothing to do
# with the peer-hosted DKS plane under test. The positive assertion: every
# Ready kubelet-backed node carries a flannel.alpha.coreos.com/public-ip
# annotation whose value is inside the Tailscale CGNAT range (100.64/10) —
# i.e. flannel is running AND its VXLAN underlay is the tailnet, which is
# exactly the ADR Option A topology.
#
# A cluster that demonstrably is NOT that (no flannel anywhere, or flannel
# over a non-tailnet underlay) is an OBSERVED contradiction of the venue this
# gate claims to be testing -> FAIL, not BLOCKED. Only an unanswerable query
# (no node list at all) is BLOCKED.
# ---------------------------------------------------------------------------
if dks_selected peer-venue; then
    if [ "$NODE_QUERY_OK" != "1" ]; then
        dks_record peer-venue BLOCKED "the node-list query COULD NOT RUN (rc=$NODE_QUERY_RC): ${NODE_QUERY_ERR:-no stderr}; the venue could not be identified either way"
    elif [ "$READY_COUNT" -lt 1 ]; then
        dks_record peer-venue BLOCKED "no Ready kubelet-backed node to inspect; the venue could not be identified either way"
    # The rc is CHECKED. Previously this query discarded stderr AND rc, so an
    # RBAC denial or a transient API error yielded VENUE_SEEN=0 -> BLOCKED,
    # indistinguishable from a cluster that answered and had no annotations —
    # and the verdict then proceeded to green past the skipped venue check.
    elif ! dks_run kubectl get nodes $READY_LIST -o 'jsonpath={range .items[*]}{.metadata.name}{" "}{.metadata.annotations.flannel\.alpha\.coreos\.com/public-ip}{"\n"}{end}'
    then
        dks_record peer-venue BLOCKED "the node-annotation query COULD NOT RUN (rc=$DKS_RUN_RC): ${DKS_RUN_ERR:-no stderr}; the venue could not be identified either way — this is NOT 'the nodes carry no flannel annotation'"
    else
        VENUE_ANN="$DKS_RUN_OUT"
        VENUE_SEEN=0; VENUE_OK=""; VENUE_BAD=""
        while read -r vnode vann _vrest; do
            [ -z "${vnode:-}" ] && continue
            VENUE_SEEN=$((VENUE_SEEN + 1))
            if [ -z "${vann:-}" ]; then
                VENUE_BAD="$VENUE_BAD $vnode:no-flannel-public-ip-annotation"
            elif dks_is_tailnet_ip "$vann"; then
                VENUE_OK="$VENUE_OK $vnode:flannel-public-ip=$vann"
            else
                VENUE_BAD="$VENUE_BAD $vnode:flannel-public-ip-not-tailnet($vann)"
            fi
        done <<VENUE_EOF
$VENUE_ANN
VENUE_EOF
        if [ "$VENUE_SEEN" -eq 0 ]; then
            dks_record peer-venue BLOCKED "the node-annotation query returned nothing for: $READY_LIST; the venue could not be identified either way"
        elif [ -n "$VENUE_BAD" ]; then
            dks_record peer-venue FAIL "this is NOT a peer-hosted DKS plane (ADR Option A: flannel VXLAN over a tailnet underlay); observed contradictions:$VENUE_BAD; nodes that did qualify:${VENUE_OK:- none}. Every other result in this run is about some OTHER venue."
        else
            dks_record peer-venue PASS "peer-hosted flannel plane over a tailnet underlay on all $VENUE_SEEN Ready kubelet-backed node(s):$VENUE_OK"
        fi
    fi
fi

# ---------------------------------------------------------------------------
# 3. distinct-pod-cidrs  (the B5 acceptance)
# PRECONDITION — NOT evidence about the slice under test, despite having been
# classified Substantive for five sprints. It reads .spec.podCIDR from the API
# server and nothing else: no probe pod, no host inspection, not one packet.
# Distinct per-node podCIDRs are IPAM bookkeeping k3s assigns at node
# registration under its default --allocate-node-cidrs, so they hold whether or
# not flannel runs, whether or not tailscale0 exists, and whether or not a byte
# can cross between nodes. Applying the header's classification rule — "if the
# cluster were completely broken but the API server healthy, could this still
# pass?" — the answer is YES, so it cannot carry a verdict.
#
# That mattered: it was the ONLY substantive check that survived a degraded
# environment, which made it exactly the check that still passed when every
# other check blocked (SUMMARY pass=2 fail=0 blocked=10 -> RESULT OK -> exit 0).
# It is still run and still reported — a duplicate/absent podCIDR is a real
# defect worth FAILing on — it just no longer makes a run green.
# Scoped to Ready kubelet-backed nodes only (stale NotReady nodes excluded).
# ---------------------------------------------------------------------------
if dks_selected distinct-pod-cidrs; then
    # Query only Ready kubelet-backed nodes (k3s/v1.*, exclude vknode).
    # custom-columns (not jsonpath): jsonpath emits NOTHING for absent
    # .spec.podCIDR, which shifts columns and makes missing CIDR masquerade as
    # the next field; custom-columns prints literal <none>.
    #
    # rc CHECKED: an unchecked rc here turned a denied query into the confident
    # (and wrong) "no Ready kubelet-backed nodes returned a .spec.podCIDR".
    CIDR_QUERY_OK=1
    if dks_run kubectl get nodes --no-headers -o \
        'custom-columns=NAME:.metadata.name,READY:.status.conditions[?(@.type=="Ready")].status,PODCIDR:.spec.podCIDR,VER:.status.nodeInfo.kubeletVersion'
    then
        NODE_TABLE="$DKS_RUN_OUT"
    else
        CIDR_QUERY_OK=0; NODE_TABLE=""
    fi
    # One query, split three ways: the scope (Ready, kubelet-backed) and both
    # exclusion NOTE lines all derive from the same snapshot, so they cannot
    # disagree with each other.
    CIDR_LINES="$(printf '%s\n' "$NODE_TABLE" | awk '$2=="True" && $4 !~ /vknode/ && $4 ~ /k3s|^v1\./ {print $1, $3}')"
    EXCLUDED_VIRTUAL="$(printf '%s\n' "$NODE_TABLE" | awk '$4 ~ /vknode/ {printf "%s ", $1}')"
    EXCLUDED_NOTREADY="$(printf '%s\n' "$NODE_TABLE" | awk 'NF>0 && $2!="True" && $4 !~ /vknode/ {printf "%s ", $1}')"
    [ -n "$EXCLUDED_VIRTUAL" ] && echo "NOTE excluded from distinct-pod-cidrs (virtual-kubelet): $EXCLUDED_VIRTUAL"
    [ -n "$EXCLUDED_NOTREADY" ] && echo "NOTE excluded from distinct-pod-cidrs (NotReady): $EXCLUDED_NOTREADY"
    if [ "$CIDR_QUERY_OK" != "1" ]; then
        dks_record distinct-pod-cidrs BLOCKED "the node podCIDR query COULD NOT RUN (rc=$DKS_RUN_RC): ${DKS_RUN_ERR:-no stderr}; this is NOT 'no node reported a podCIDR'"
    elif [ -z "$CIDR_LINES" ]; then
        dks_record distinct-pod-cidrs BLOCKED "no Ready kubelet-backed nodes returned a .spec.podCIDR field"
    else
        REASON="$(echo "$CIDR_LINES" | dks_distinct_cidrs)"
        if [ $? -eq 0 ]; then
            dks_record distinct-pod-cidrs PASS "$REASON"
        else
            dks_record distinct-pod-cidrs FAIL "$REASON"
        fi
    fi
fi

# ---------------------------------------------------------------------------
# 4. flannel-iface — flannel pinned to tailscale0 on BOTH selected workers.
# PASS requires, for NODE_A AND NODE_B, all four evidence items:
#   - flannel public-ip annotation present and inside 100.64/10;
#   - host k3s agent argv contains --flannel-iface=tailscale0;
#   - host tailscale0 carries an IPv4 address;
#   - that annotation EQUALS the observed tailscale0 IPv4 on that SAME node.
# The equality item exists because the annotation and the interface can each
# individually look fine (both present, both tailnet-shaped) while naming two
# DIFFERENT tailnet addresses on the same node — e.g. flannel's public-ip was
# stamped from a stale/second tailscale0 address. That is not proof flannel is
# actually pinned to the observed interface, so it must not PASS.
# Host evidence comes from hostPID/hostNetwork inspection pods
# (DKS_ALLOW_NODE_DEBUG=1, OBSERVED) or an operator-supplied DKS_HOST_EVIDENCE
# file (ATTESTED). ANY missing item on EITHER node => BLOCKED, never PASS.
# Only observed contradictory values (non-tailnet annotation, argv without the
# flag, tailscale0 present without IPv4, annotation != tailscale0 IPv4) =>
# FAIL. An otherwise-passing result that leans on ANY attested item =>
# ATTESTED, never PASS: nobody inspected that machine.
# ---------------------------------------------------------------------------
if dks_selected flannel-iface; then
    if [ "$TWO_NODES" != "1" ]; then
        dks_record flannel-iface BLOCKED "$NO_TWO"
    # rc CHECKED: unchecked, a denied query produced an empty annotation pair
    # and the harness then announced "flannel is not running on the selected
    # nodes" — a diagnosis about a query that never ran.
    elif ! dks_run kubectl get nodes "$NODE_A" "$NODE_B" -o 'jsonpath={range .items[*]}{.metadata.name}{" "}{.metadata.annotations.flannel\.alpha\.coreos\.com/public-ip}{"\n"}{end}'
    then
        dks_record flannel-iface BLOCKED "the node-annotation query COULD NOT RUN (rc=$DKS_RUN_RC): ${DKS_RUN_ERR:-no stderr}; this is NOT 'flannel is not running'"
    else
        ANN="$DKS_RUN_OUT"
        ANN_A="$(printf '%s\n' "$ANN" | awk -v n="$NODE_A" '$1==n {print $2; exit}')"
        ANN_B="$(printf '%s\n' "$ANN" | awk -v n="$NODE_B" '$1==n {print $2; exit}')"
        if [ -z "$ANN_A" ] && [ -z "$ANN_B" ]; then
            dks_record flannel-iface BLOCKED "no flannel.alpha.coreos.com/public-ip annotation on $NODE_A or $NODE_B; flannel is not running on the selected nodes"
        else
            FI_FAIL=""; FI_MISSING=""; FI_OK=""; FI_ATTESTED=""
            FI_IDX=0
            for node in "$NODE_A" "$NODE_B"; do
                ann="$ANN_A"; [ "$FI_IDX" = "1" ] && ann="$ANN_B"
                # The annotation is read from the API server by this harness:
                # always OBSERVED.
                if [ -z "$ann" ]; then
                    FI_MISSING="$FI_MISSING $node:annotation-absent"
                elif dks_is_tailnet_ip "$ann"; then
                    FI_OK="$FI_OK $node:annotation=$ann(observed)"
                else
                    FI_FAIL="$FI_FAIL $node:annotation-not-tailnet($ann)(observed)"
                fi
                # NOT $(dks_inspect ...): the CREATED registration must reach
                # this shell, not die in a command-substitution subshell.
                INSPECT_BLOB=""
                if [ "${DKS_ALLOW_NODE_DEBUG:-0}" = "1" ]; then
                    dks_inspect flannel "$FI_IDX" "$node" "$FLANNEL_INSPECT_SCRIPT" || true
                    INSPECT_BLOB="$DKS_INSPECT_OUT"
                fi
                dks_resolve_ev "$INSPECT_BLOB" "$node" k3s_argv || true
                EV_ARGV="$DKS_EV_VALUE"; SRC_ARGV="$DKS_EV_SOURCE"
                dks_resolve_ev "$INSPECT_BLOB" "$node" tailscale0 || true
                EV_TS0="$DKS_EV_VALUE"; SRC_TS0="$DKS_EV_SOURCE"
                case "$EV_ARGV" in
                    flannel-iface-tailscale0)
                        FI_OK="$FI_OK $node:argv-ok($SRC_ARGV)"
                        [ "$SRC_ARGV" = "attested" ] && FI_ATTESTED="$FI_ATTESTED $node:k3s_argv"
                        ;;
                    no-flannel-iface)
                        FI_FAIL="$FI_FAIL $node:k3s-agent-argv-lacks-flannel-iface-tailscale0($SRC_ARGV)" ;;
                    *) FI_MISSING="$FI_MISSING $node:k3s-argv-${EV_ARGV:-no-evidence}" ;;
                esac
                ts0_ip=""
                case "$EV_TS0" in
                    ipv4:*)
                        ts0_ip="${EV_TS0#ipv4:}"
                        FI_OK="$FI_OK $node:tailscale0-$EV_TS0($SRC_TS0)"
                        [ "$SRC_TS0" = "attested" ] && FI_ATTESTED="$FI_ATTESTED $node:tailscale0"
                        ;;
                    absent|no-ipv4) FI_FAIL="$FI_FAIL $node:tailscale0-$EV_TS0($SRC_TS0)" ;;
                    *) FI_MISSING="$FI_MISSING $node:tailscale0-${EV_TS0:-no-evidence}" ;;
                esac
                # Per-node equality: only meaningful once BOTH the annotation
                # and the tailscale0 IPv4 are independently present on this
                # SAME node. A mismatch here is an observed contradiction
                # (FAIL), not missing evidence -- both values were read, they
                # just disagree. Its provenance is that of its weaker half:
                # an equality resting on an attested tailscale0 is attested.
                if [ -n "$ann" ] && [ -n "$ts0_ip" ]; then
                    if [ "$ann" != "$ts0_ip" ]; then
                        FI_FAIL="$FI_FAIL $node:annotation-vs-tailscale0-mismatch(annotation=$ann,tailscale0=$ts0_ip,tailscale0-source=$SRC_TS0)"
                    else
                        FI_OK="$FI_OK $node:annotation-equals-tailscale0($SRC_TS0)"
                    fi
                fi
                FI_IDX=$((FI_IDX + 1))
            done
            if [ -n "$FI_FAIL" ]; then
                dks_record flannel-iface FAIL "observed contradictions:$FI_FAIL"
            elif [ -n "$FI_MISSING" ]; then
                dks_record flannel-iface BLOCKED "missing evidence:$FI_MISSING; set DKS_ALLOW_NODE_DEBUG=1 (hostPID/hostNetwork inspection pods) or supply DKS_HOST_EVIDENCE=<file>"
            elif [ -n "$FI_ATTESTED" ]; then
                dks_record flannel-iface ATTESTED "operator-attested, NOT observed by this harness — no machine was inspected for:$FI_ATTESTED; re-run with DKS_ALLOW_NODE_DEBUG=1 to turn this into evidence. Items:$FI_OK"
            else
                dks_record flannel-iface PASS "both $NODE_A and $NODE_B verified:$FI_OK"
            fi
        fi
    fi
fi

# ---------------------------------------------------------------------------
# 5. no-stale-conflist — /host/etc/cni/net.d on BOTH selected workers, read
# through the inspection pod's read-only hostPath mount (an ordinary pod has
# no /host and would report the container's own empty filesystem as truth).
# Unreadable / no evidence => BLOCKED; an observed stale conflist => FAIL;
# a clean result resting on operator-supplied evidence => ATTESTED, not PASS.
# ---------------------------------------------------------------------------
if dks_selected no-stale-conflist; then
    if [ "$TWO_NODES" != "1" ]; then
        dks_record no-stale-conflist BLOCKED "$NO_TWO"
    else
        CF_STALE=""; CF_MISSING=""; CF_CLEAN=""; CF_ATTESTED=""
        CF_IDX=0
        for node in "$NODE_A" "$NODE_B"; do
            INSPECT_BLOB=""
            if [ "${DKS_ALLOW_NODE_DEBUG:-0}" = "1" ]; then
                dks_inspect conflist "$CF_IDX" "$node" "$CONFLIST_INSPECT_SCRIPT" || true
                INSPECT_BLOB="$DKS_INSPECT_OUT"
            fi
            dks_resolve_ev "$INSPECT_BLOB" "$node" cni_confdir || true
            EV_CNI="$DKS_EV_VALUE"; SRC_CNI="$DKS_EV_SOURCE"
            case "$EV_CNI" in
                listing:*10-outpost.conflist*|listing:*10-bridge.conflist*)
                    CF_STALE="$CF_STALE $node(${EV_CNI#listing:})($SRC_CNI)" ;;
                listing:*|empty|dir-absent)
                    CF_CLEAN="$CF_CLEAN $node($SRC_CNI)"
                    [ "$SRC_CNI" = "attested" ] && CF_ATTESTED="$CF_ATTESTED $node:cni_confdir"
                    ;;
                *)
                    CF_MISSING="$CF_MISSING $node:${EV_CNI:-no-evidence}" ;;
            esac
            CF_IDX=$((CF_IDX + 1))
        done
        if [ -n "$CF_STALE" ]; then
            dks_record no-stale-conflist FAIL "stale 10-outpost/10-bridge conflist on:$CF_STALE"
        elif [ -n "$CF_MISSING" ]; then
            dks_record no-stale-conflist BLOCKED "/host/etc/cni/net.d unreadable or no evidence on:$CF_MISSING; set DKS_ALLOW_NODE_DEBUG=1 (hostPath inspection pods) or supply DKS_HOST_EVIDENCE=<file>"
        elif [ -n "$CF_ATTESTED" ]; then
            dks_record no-stale-conflist ATTESTED "operator-attested, NOT observed by this harness — no machine was inspected for:$CF_ATTESTED; re-run with DKS_ALLOW_NODE_DEBUG=1 to turn this into evidence. Items:$CF_CLEAN"
        else
            dks_record no-stale-conflist PASS "no stale 10-outpost/10-bridge conflist on:$CF_CLEAN"
        fi
    fi
fi

# ---------------------------------------------------------------------------
# 6. cross-node-pod-ip — THE headline check.
# ---------------------------------------------------------------------------
POD_A="${RUN_ID}-a"; POD_B="${RUN_ID}-b"; POD_B_IP=""
# PROBE_READY is the single source of truth for "a usable probe pod exists on
# each node". Every check that needs one gates on it, so a check whose probe
# was never created reports BLOCKED rather than FAIL.
PROBE_READY=0
if [ "$TWO_NODES" = "1" ] && { dks_selected cross-node-pod-ip || dks_selected service-clusterip \
        || dks_selected cluster-dns || dks_selected logs-exec || dks_selected headlamp; }; then
    make_pod "$POD_A" "$NODE_A"; make_pod "$POD_B" "$NODE_B"
    ERR_A="$(wait_ready "$POD_A")"; ERR_B="$(wait_ready "$POD_B")"
    POD_B_IP="$(k get pod "$POD_B" -o jsonpath='{.status.podIP}' 2>/dev/null)"
    [ -z "$ERR_A" ] && [ -z "$ERR_B" ] && PROBE_READY=1
fi

if dks_selected cross-node-pod-ip; then
    if [ "$TWO_NODES" != "1" ]; then
        dks_record cross-node-pod-ip BLOCKED "$NO_TWO"
    elif [ -n "${ERR_A:-}" ] || [ -n "${ERR_B:-}" ]; then
        dks_record cross-node-pod-ip BLOCKED "probe pods did not start: ${ERR_A:-}${ERR_B:-}"
    elif [ -z "$POD_B_IP" ]; then
        dks_record cross-node-pod-ip BLOCKED "pod on $NODE_B has no podIP"
    else
        # The exec's RETURN CODE is read, and the assertion on the output is
        # POSITIVE (the specific success string ping prints). A pipeline here
        # would discard the rc -- trim afterwards, never inline.
        OUT="$(k exec "$POD_A" -- sh -c "ping -c 3 -W 3 $POD_B_IP" 2>&1)"; RC=$?
        OUT="$(printf '%s\n' "$OUT" | tail -4)"
        if [ "$RC" -eq 0 ] && printf '%s\n' "$OUT" | grep -qE ' 0% packet loss'; then
            dks_record cross-node-pod-ip PASS "$POD_A@$NODE_A -> $POD_B_IP@$NODE_B: $(printf '%s\n' "$OUT" | grep 'packet loss')"
        else
            dks_record cross-node-pod-ip FAIL "$POD_A@$NODE_A -> $POD_B_IP@$NODE_B (exec rc=$RC): $OUT"
        fi
    fi
fi

# ---------------------------------------------------------------------------
# 7. service-clusterip
# The backend serves /etc, so GET /hostname returns the BACKEND POD'S OWN
# hostname — which kubelet sets to the pod name. That gives this check a
# POSITIVE content assertion: the bytes must be the ones only the pod on
# NODE_B could have produced. The previous negative blacklist
# ("PASS unless the output mentions refused|timed out|no route|error") scored
# busybox's own "Network is unreachable" as a PASS, because that phrase is on
# no blacklist -- the PASS line quoted the error text verbatim. A negative
# blacklist can only ever enumerate the failures someone thought of; it treats
# every unanticipated failure as success. The exec's rc is read too.
# ---------------------------------------------------------------------------
SVC="${RUN_ID}-svc"
if dks_selected service-clusterip || dks_selected cluster-dns; then
    if [ "$TWO_NODES" != "1" ] || [ -n "${ERR_B:-}" ]; then
        SVC_IP=""
    else
        CREATED+=("svc/$SVC")  # registered BEFORE creation, same rule as pods
        cat <<YAML | k apply -f - >/dev/null 2>&1
apiVersion: v1
kind: Service
metadata: {name: $SVC}
spec:
  selector: {app: $POD_B}
  ports: [{port: 8080, targetPort: 8080}]
YAML
        k exec "$POD_B" -- sh -c 'nohup httpd -f -p 8080 -h /etc >/dev/null 2>&1 &' >/dev/null 2>&1
        sleep 3
        SVC_IP="$(k get svc "$SVC" -o jsonpath='{.spec.clusterIP}' 2>/dev/null)"
    fi
fi
if dks_selected service-clusterip; then
    if [ "$TWO_NODES" != "1" ]; then
        dks_record service-clusterip BLOCKED "$NO_TWO"
    elif [ -n "${ERR_A:-}" ]; then
        # The check execs FROM POD_A; a not-Ready POD_A makes kubectl exec
        # itself fail (e.g. "container not running"). Missing precondition ->
        # BLOCKED: nothing about clusterIP routing was exercised either way.
        dks_record service-clusterip BLOCKED "source probe pod on $NODE_A did not become Ready: $ERR_A"
    elif [ -z "${SVC_IP:-}" ]; then
        dks_record service-clusterip BLOCKED "Service got no clusterIP (backend pod on $NODE_B unavailable)"
    else
        OUT="$(k exec "$POD_A" -- sh -c "wget -q -T 8 -O - http://$SVC_IP:8080/hostname" 2>&1)"; RC=$?
        OUT="$(printf '%s\n' "$OUT" | head -c 200)"
        # POSITIVE assertion: the payload must be the backend pod's own
        # hostname. Anything else -- an error string, an empty body, a reply
        # from some other endpoint -- is a FAIL.
        if [ "$RC" -eq 0 ] && printf '%s\n' "$OUT" | grep -qF "$POD_B"; then
            dks_record service-clusterip PASS "$POD_A -> clusterIP $SVC_IP:8080/hostname returned the backend pod's own hostname ($OUT), proving the request reached $POD_B@$NODE_B"
        else
            dks_record service-clusterip FAIL "$POD_A -> clusterIP $SVC_IP:8080/hostname (exec rc=$RC) did not return $POD_B's hostname: ${OUT:-empty response}"
        fi
    fi
fi

# ---------------------------------------------------------------------------
# 8. cluster-dns
# ---------------------------------------------------------------------------
if dks_selected cluster-dns; then
    if [ "$TWO_NODES" != "1" ]; then
        dks_record cluster-dns BLOCKED "$NO_TWO"
    elif [ -n "${ERR_A:-}" ]; then
        # Same reasoning as service-clusterip: nslookup runs FROM POD_A, so a
        # not-Ready POD_A must BLOCK, not false-FAIL on an exec error.
        dks_record cluster-dns BLOCKED "source probe pod on $NODE_A did not become Ready: $ERR_A"
    elif [ -z "${SVC_IP:-}" ]; then
        dks_record cluster-dns BLOCKED "no Service to resolve (backend pod on $NODE_B unavailable)"
    else
        # rc AND a positive content assertion (the exact clusterIP).
        OUT="$(k exec "$POD_A" -- sh -c "nslookup $SVC" 2>&1)"; RC=$?
        OUT="$(printf '%s\n' "$OUT" | tail -6)"
        if [ "$RC" -eq 0 ] && printf '%s\n' "$OUT" | grep -qF "$SVC_IP"; then
            dks_record cluster-dns PASS "short name $SVC resolved to $SVC_IP from $POD_A@$NODE_A"
        else
            dks_record cluster-dns FAIL "short name $SVC did not resolve to $SVC_IP (exec rc=$RC): $OUT"
        fi
    fi
fi

# ---------------------------------------------------------------------------
# 9. logs-exec against NODE_B (the second selected node).
# NOTE: this harness does NOT verify how the API server reaches NODE_B's
# kubelet, so it must not claim NODE_B is "the remote/tunnelled node" -- that
# is an unverified property of the venue, not something this check observes.
# ---------------------------------------------------------------------------
if dks_selected logs-exec; then
    if [ "$TWO_NODES" != "1" ]; then
        dks_record logs-exec BLOCKED "$NO_TWO"
    elif [ -n "${ERR_B:-}" ]; then
        dks_record logs-exec BLOCKED "pod on $NODE_B did not start: ${ERR_B}"
    else
        # Both return codes are read; both assertions are positive (the exact
        # strings only a live pod on NODE_B could have produced).
        LOGS="$(k logs "$POD_B" --tail=3 2>&1)"; LOGS_RC=$?
        LOGS="$(printf '%s\n' "$LOGS" | head -c 200)"
        EXECO="$(k exec "$POD_B" -- sh -c 'echo dks-exec-ok' 2>&1)"; EXEC_RC=$?
        EXECO="$(printf '%s\n' "$EXECO" | head -c 200)"
        if [ "$LOGS_RC" -eq 0 ] && [ "$EXEC_RC" -eq 0 ] \
            && printf '%s\n' "$LOGS" | grep -q 'dks-alive' \
            && printf '%s\n' "$EXECO" | grep -q 'dks-exec-ok'; then
            dks_record logs-exec PASS "logs+exec ok against $POD_B@$NODE_B (logs=$(printf '%s\n' "$LOGS" | head -1))"
        else
            dks_record logs-exec FAIL "logs rc=$LOGS_RC [$LOGS] exec rc=$EXEC_RC [$EXECO] against $POD_B@$NODE_B"
        fi
    fi
fi

# ---------------------------------------------------------------------------
# 10. headlamp
# Headlamp is a PRECONDITION of this check, not its subject: the check asserts
# that an in-cluster Service is reachable from a pod on the other node. So a
# Headlamp that is absent, unlabelled, not Running, or has no clusterIP is
# missing evidence -> BLOCKED. Only an actually-unreachable Running Headlamp
# Service is a FAIL.
# ---------------------------------------------------------------------------
if dks_selected headlamp; then
    HL_NS="${DKS_HEADLAMP_NS:-}"; HL_SVC="${DKS_HEADLAMP_SVC:-}"
    if [ -z "$HL_NS" ] || [ -z "$HL_SVC" ]; then
        FOUND="$(kubectl get svc -A -o 'jsonpath={range .items[*]}{.metadata.namespace}{" "}{.metadata.name}{"\n"}{end}' 2>/dev/null | grep -i headlamp | head -1)"
        HL_NS="${FOUND%% *}"; HL_SVC="${FOUND##* }"
    fi
    if [ -z "$HL_NS" ] || [ -z "$HL_SVC" ]; then
        dks_record headlamp BLOCKED "no Headlamp Service found and DKS_HEADLAMP_NS/DKS_HEADLAMP_SVC unset; this harness does not deploy Headlamp"
    else
        # rc CHECKED on the pod lookup: unchecked, a denied query looked exactly
        # like "no pod carries the headlamp label" and was reported as such.
        HL_QUERY_OK=1
        if dks_run kubectl -n "$HL_NS" get pods -l 'app.kubernetes.io/name=headlamp' -o jsonpath='{.items[0].status.phase}'; then
            HL_POD="$DKS_RUN_OUT"
        else
            HL_QUERY_OK=0; HL_POD=""; HL_QUERY_ERR="$DKS_RUN_ERR"; HL_QUERY_RC="$DKS_RUN_RC"
        fi
        HL_IP="$(kubectl -n "$HL_NS" get svc "$HL_SVC" -o jsonpath='{.spec.clusterIP}' 2>/dev/null)"
        HL_PORT="$(kubectl -n "$HL_NS" get svc "$HL_SVC" -o jsonpath='{.spec.ports[0].port}' 2>/dev/null)"
        if [ "$HL_QUERY_OK" != "1" ]; then
            dks_record headlamp BLOCKED "the Headlamp pod query COULD NOT RUN (rc=$HL_QUERY_RC): ${HL_QUERY_ERR:-no stderr}; this is NOT 'no pod carries the headlamp label'"
        elif [ -z "$HL_POD" ]; then
            # No pod matched the selector. That is almost always a deployment
            # that does not carry the conventional label, i.e. THIS HARNESS
            # CANNOT SEE the backend -- missing evidence, not a defect of the
            # pod network. BLOCKED, and name the label so it is fixable.
            dks_record headlamp BLOCKED "no pod in namespace $HL_NS carries the label app.kubernetes.io/name=headlamp, so this harness cannot confirm a Headlamp backend exists behind svc $HL_SVC; label the deployment or point DKS_HEADLAMP_NS/DKS_HEADLAMP_SVC at one that is labelled"
        elif [ "$HL_POD" != "Running" ]; then
            dks_record headlamp BLOCKED "Headlamp svc $HL_NS/$HL_SVC exists but its labelled pod is not Running (phase=$HL_POD); nothing about Service reachability was exercised"
        elif [ -z "$HL_IP" ] || [ -z "$HL_PORT" ]; then
            dks_record headlamp BLOCKED "svc $HL_NS/$HL_SVC has no clusterIP/port to reach (clusterIP=${HL_IP:-none} port=${HL_PORT:-none})"
        elif [ "$PROBE_READY" != "1" ]; then
            dks_record headlamp BLOCKED "Headlamp scheduled ($HL_NS/$HL_SVC) but no in-cluster probe pod was available to reach its Service (probe pods not created or not Ready); $NO_TWO"
        else
            OUT="$(k exec "$POD_A" -- sh -c "wget -q -T 8 -S -O /dev/null http://$HL_IP:$HL_PORT/ 2>&1 | head -3" 2>&1)"
            # POSITIVE assertion: a well-formed HTTP status line, which only a
            # reachable HTTP server can produce, and which is exactly what this
            # check claims. The exec rc is deliberately NOT required to be 0
            # here (wget exits non-zero on any non-2xx, and a 404 from / still
            # proves reachability); the status line IS the evidence, so this is
            # not a negative blacklist -- there is no "anything unrecognised
            # counts as success" path.
            CODE="$(printf '%s\n' "$OUT" | grep -oE 'HTTP/1\.[01] [0-9]{3}' | head -1)"
            if [ -n "$CODE" ]; then
                dks_record headlamp PASS "Headlamp $HL_NS/$HL_SVC at $HL_IP:$HL_PORT reachable ($CODE from /)"
            else
                dks_record headlamp FAIL "Headlamp $HL_NS/$HL_SVC at $HL_IP:$HL_PORT returned no HTTP status line: $OUT"
            fi
        fi
    fi
fi

# ---------------------------------------------------------------------------
# 11. nanochat — cross-node placement of a real workload
# ---------------------------------------------------------------------------
if dks_selected nanochat; then
    if [ -z "${DKS_NANOCHAT_IMAGE:-}" ]; then
        dks_record nanochat BLOCKED "DKS_NANOCHAT_IMAGE unset; no nanochat image is published in this repo and this harness will not substitute a weaker workload"
    elif [ "$TWO_NODES" != "1" ]; then
        dks_record nanochat BLOCKED "$NO_TWO"
    else
        NC="${RUN_ID}-nanochat"
        CREATED+=("deploy/$NC")  # registered BEFORE creation
        cat <<YAML | k apply -f - >/dev/null 2>&1
apiVersion: apps/v1
kind: Deployment
metadata: {name: $NC}
spec:
  replicas: 2
  selector: {matchLabels: {app: $NC}}
  template:
    metadata: {labels: {app: $NC}}
    spec:
      topologySpreadConstraints:
      - maxSkew: 1
        topologyKey: kubernetes.io/hostname
        whenUnsatisfiable: DoNotSchedule
        labelSelector: {matchLabels: {app: $NC}}
      containers: [{name: nanochat, image: $DKS_NANOCHAT_IMAGE}]
YAML
        # Bounded on DKS_TIMEOUT via the deployment's own rollout status,
        # not a fixed sleep: a fixed sleep 10 either wastes time on a fast
        # pull or false-FAILs a slow one long before DKS_TIMEOUT is reached.
        k rollout status "deployment/$NC" --timeout="${DKS_TIMEOUT}s" >/dev/null 2>&1
        ROLLOUT_RC=$?
        PLACED="$(k get pods -l "app=$NC" -o jsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}' 2>/dev/null | sort -u | wc -l | tr -d ' ')"
        READYN="$(k get pods -l "app=$NC" --field-selector=status.phase=Running -o name 2>/dev/null | wc -l | tr -d ' ')"
        if [ "$ROLLOUT_RC" -eq 0 ] && [ "$PLACED" -ge 2 ] && [ "$READYN" -ge 2 ]; then
            dks_record nanochat PASS "2 nanochat replicas Running across $PLACED distinct nodes"
        else
            # An image that never became pullable within DKS_TIMEOUT is a
            # missing precondition (the workload was never proven either way),
            # not a contradiction of the ADR's cross-node placement claim --
            # distinguish it from a real scheduling/placement defect so a slow
            # registry doesn't masquerade as a topology-spread failure.
            PULL_ISSUE="$(k get pods -l "app=$NC" -o jsonpath='{range .items[*]}{.status.containerStatuses[0].state.waiting.reason}{"\n"}{end}' 2>/dev/null | grep -E 'ImagePullBackOff|ErrImagePull' | head -1)"
            if [ -n "$PULL_ISSUE" ]; then
                dks_record nanochat BLOCKED "DKS_NANOCHAT_IMAGE ($DKS_NANOCHAT_IMAGE) not pullable within ${DKS_TIMEOUT}s: $PULL_ISSUE"
            else
                dks_record nanochat FAIL "nanochat placed on $PLACED node(s), $READYN Running: $(k get pods -l "app=$NC" -o wide 2>&1 | tail -3)"
            fi
        fi
    fi
fi

# ---------------------------------------------------------------------------
# 12. bashy-chunked
# ---------------------------------------------------------------------------
if dks_selected bashy-chunked; then
    if [ -z "${DKS_BASHY_IMAGE:-}" ]; then
        dks_record bashy-chunked BLOCKED "DKS_BASHY_IMAGE unset; no chunked-bashy container image or Job manifest exists in this repo, and there is no deterministic substitute for a distributed chunked workload"
    elif [ "$TWO_NODES" != "1" ]; then
        dks_record bashy-chunked BLOCKED "$NO_TWO"
    else
        BJ="${RUN_ID}-bashy"
        CREATED+=("job/$BJ")  # registered BEFORE creation
        cat <<YAML | k apply -f - >/dev/null 2>&1
apiVersion: batch/v1
kind: Job
metadata: {name: $BJ}
spec:
  completions: 4
  parallelism: 2
  completionMode: Indexed
  template:
    spec:
      restartPolicy: Never
      containers: [{name: bashy, image: $DKS_BASHY_IMAGE}]
YAML
        k wait --for=condition=complete "job/$BJ" --timeout="${DKS_TIMEOUT}s" >/dev/null 2>&1
        SUCC="$(k get job "$BJ" -o jsonpath='{.status.succeeded}' 2>/dev/null)"
        NODESN="$(k get pods -l "job-name=$BJ" -o jsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}' 2>/dev/null | sort -u | wc -l | tr -d ' ')"
        if [ "${SUCC:-0}" = "4" ] && [ "$NODESN" -ge 2 ]; then
            dks_record bashy-chunked PASS "4/4 indexed chunks completed across $NODESN nodes"
        else
            # Same carve-out as nanochat: an image that never became pullable
            # means the distributed workload never ran, so nothing was proven
            # either way. BLOCKED (naming the image), not FAIL.
            PULL_ISSUE="$(k get pods -l "job-name=$BJ" -o jsonpath='{range .items[*]}{.status.containerStatuses[0].state.waiting.reason}{"\n"}{end}' 2>/dev/null | grep -E 'ImagePullBackOff|ErrImagePull' | head -1)"
            if [ -n "$PULL_ISSUE" ]; then
                dks_record bashy-chunked BLOCKED "DKS_BASHY_IMAGE ($DKS_BASHY_IMAGE) not pullable within ${DKS_TIMEOUT}s: $PULL_ISSUE"
            else
                dks_record bashy-chunked FAIL "succeeded=${SUCC:-0}/4 across $NODESN node(s)"
            fi
        fi
    fi
fi

echo
dks_summary
exit $?
