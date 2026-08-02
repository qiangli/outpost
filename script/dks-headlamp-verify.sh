#!/usr/bin/env bash
# dks-headlamp-verify.sh — verify a Headlamp operating UI deployed on a
# peer-hosted DKS control plane (internal/agent/headlamp) is:
#   (a) RUNNING       — the deployment has at least one ready replica;
#   (b) ANSWERING     — an HTTP status line comes back over the operator's
#                       loopback-pinned port-forward (the intended local path);
#   (c) TOKEN-GATED   — an UNAUTHENTICATED request is positively shown NOT to
#                       yield cluster authority (the token-login boundary is
#                       proven, never assumed);
#   (d) NOT EXPOSED   — no unauthenticated network path answers: the Service is
#                       ClusterIP-only, the pod ServiceAccount holds zero RBAC
#                       grants, and a live TCP probe of every discovered node
#                       address (ALL types: InternalIP, ExternalIP, Hostname)
#                       finds the Headlamp ports unreachable.
#
# Emits one machine-readable line per check:
#     CHECK <name> PASS|FAIL|BLOCKED|NOT-RUN <detail>
# Exit status (same contract as script/dks-peer-acceptance.sh):
#     0  OK           — every required check ran and PASSed
#     1  FAIL         — at least one check FAILed
#     2  INCONCLUSIVE — any check was BLOCKED (a blocked security assurance
#                       is not OK), any required check NEVER RAN (a check
#                       that did not run is not a pass), or no check PASSed
#
# REQUIRED-CHECK INVARIANT — every check in HLV_REQUIRED_CHECKS must produce a
# verdict. HLV_ONLY exists for focused debugging; a subset run closes each
# unselected check as NOT-RUN, so CI invoking a subset can never report green
# forever. A success state is never reached by the ABSENCE of evidence.
#
# EVIDENCE INVARIANT — the negative (no-unauthenticated-exposure) probe FAILS
# CLOSED:
#   * A refused connection counts as "not exposed" ONLY after a control port
#     on the SAME address answered, proving the prober works and the address
#     is a live host. A connection refused for the wrong reason (wrong host,
#     dead host, DNS failure, broken prober) is BLOCKED, never PASS.
#   * A timeout or any unclassifiable probe outcome is BLOCKED — "could not
#     determine" is never scored as "correctly not exposed".
#   * An observed OPEN Headlamp port on any node address is a FAIL even when
#     the control port could not be validated — exposure was observed.
#
# Contains NO secrets and never prints kubeconfig/token material.
#
# Usage (run from a host with LAN/tailnet reach to the plane's nodes — the
# control-plane host itself always qualifies):
#   KUBECONFIG=/path/to/kubeconfig ./script/dks-headlamp-verify.sh
#   HLV_ONLY=running,no-unauthenticated-exposure ./script/dks-headlamp-verify.sh
#
# Env:
#   KUBECONFIG          kubeconfig path (else kubectl default resolution)
#   HLV_NAMESPACE       Headlamp namespace           (default headlamp)
#   HLV_NAME            deployment/service/SA name   (default headlamp)
#   HLV_LOCAL_PORT      loopback port-forward port   (default 18466 — matches
#                       headlamp.DefaultLocalPort)
#   HLV_SERVICE_PORT    ClusterIP Service port       (default 80)
#   HLV_CONTAINER_PORT  headlamp-server's own port   (default 4466) — probed
#                       on node addresses to catch hostNetwork/hostPort drift
#   HLV_CONTROL_PORT    known-open port per node used as the positive control
#                       (default 10250 — every kubelet listens there)
#   HLV_PROBE_ADDRS     space-separated node addresses to probe instead of the
#                       discovered set. COVERAGE RULE: every discovered node
#                       must have at least one of its addresses in the
#                       override — an override that leaves any node uncovered
#                       BLOCKs the check, and when the node set cannot be
#                       enumerated an override cannot be scored at all
#                       (default: every address of every node, all types)
#   HLV_TIMEOUT         seconds for the port-forward to come up (default 30)
#   HLV_ONLY            comma-separated check names to run (default: all).
#                       Focused debugging only: a subset run closes every
#                       unselected check NOT-RUN and can never exit 0.
#
# Test hooks (OFFLINE TESTS ONLY — never set these in a real verification;
# when set, a NOTE line marks the run as not describing a real network):
#   HLV_PROBE_OVERRIDE  "host:port=open|closed|timeout|error ..." pairs
#   HLV_HTTP_OVERRIDE   "host:port[/path]=<code|none> ..." pairs (a pair
#                       without /path is the fallback for any path)
#
# Sourcing this file with HLV_LIB_ONLY=1 loads the pure helper functions
# without running any check — this is what script/dks-headlamp-verify_test.sh
# exercises offline.

set -uo pipefail

# ---------------------------------------------------------------------------
# Pure helpers (offline-testable; no kubectl, no network)
# ---------------------------------------------------------------------------

HLV_PASS_COUNT=0
HLV_FAIL_COUNT=0
HLV_BLOCKED_COUNT=0
HLV_NOTRUN_COUNT=0
HLV_RESULTS=()
# Space-delimited set of the check names that produced a verdict this run —
# the evidence hlv_close_unrun needs to tell "ran" apart from "never ran".
HLV_RECORDED=" "

# hlv_record <name> <PASS|FAIL|BLOCKED|NOT-RUN> <detail...>
hlv_record() {
    local name="$1" status="$2"
    shift 2
    local detail="$*"
    # Collapse newlines AND carriage returns so one check is always exactly
    # one line (same rationale as dks_record in the acceptance harness).
    detail="${detail//$'\r'/ }"
    detail="${detail//$'\n'/ | }"
    case "$status" in
        PASS)    HLV_PASS_COUNT=$((HLV_PASS_COUNT + 1)) ;;
        FAIL)    HLV_FAIL_COUNT=$((HLV_FAIL_COUNT + 1)) ;;
        BLOCKED) HLV_BLOCKED_COUNT=$((HLV_BLOCKED_COUNT + 1)) ;;
        NOT-RUN) HLV_NOTRUN_COUNT=$((HLV_NOTRUN_COUNT + 1)) ;;
        *)
            # An unknown status is itself a harness defect; never let it pass.
            HLV_FAIL_COUNT=$((HLV_FAIL_COUNT + 1))
            status="FAIL"
            detail="harness error: unknown status; $detail"
            ;;
    esac
    HLV_RECORDED="${HLV_RECORDED}${name} "
    HLV_RESULTS+=("CHECK $name $status $detail")
    echo "CHECK $name $status $detail"
}

# hlv_summary — prints the tally; returns the harness exit code:
#   0 OK (every required check ran and PASSed) /
#   1 FAIL (>=1 FAIL) /
#   2 INCONCLUSIVE (any BLOCKED, any NOT-RUN, or 0 PASS with 0 FAIL).
# A single passing check cannot carry the verdict after a BLOCKED check —
# a BLOCKED security assurance is worse than no check at all. And a check
# that never ran is not a pass: NOT-RUN is never green either.
hlv_summary() {
    echo "SUMMARY pass=$HLV_PASS_COUNT fail=$HLV_FAIL_COUNT blocked=$HLV_BLOCKED_COUNT notrun=$HLV_NOTRUN_COUNT"
    if [ "$HLV_FAIL_COUNT" -gt 0 ]; then
        echo "RESULT FAIL"
        return 1
    fi
    if [ "$HLV_BLOCKED_COUNT" -gt 0 ]; then
        echo "RESULT INCONCLUSIVE (${HLV_BLOCKED_COUNT} check(s) blocked; a blocked security assurance is not OK)"
        return 2
    fi
    if [ "$HLV_NOTRUN_COUNT" -gt 0 ]; then
        echo "RESULT INCONCLUSIVE (${HLV_NOTRUN_COUNT} required check(s) never ran; a check that did not run is not a pass)"
        return 2
    fi
    if [ "$HLV_PASS_COUNT" -eq 0 ]; then
        echo "RESULT INCONCLUSIVE (no check ran)"
        return 2
    fi
    echo "RESULT OK (all checks passed)"
    return 0
}

# HLV_REQUIRED_CHECKS — the checks a complete verification MUST run. A check
# that never ran is NOT a pass: HLV_ONLY exists for focused debugging, and a
# subset run closes every unselected check NOT-RUN (INCONCLUSIVE, never OK),
# so CI invoking a subset can never report green forever.
HLV_REQUIRED_CHECKS="running service-clusterip-only rbac-zero-grant answering auth-token-required no-unauthenticated-exposure"

HLV_UNRUN_CLOSED=0

# hlv_close_unrun — record NOT-RUN for every required check that produced no
# verdict this run. Idempotent; must precede every hlv_summary exit path so
# no success state is ever reached by the absence of evidence.
hlv_close_unrun() {
    [ "$HLV_UNRUN_CLOSED" = "1" ] && return 0
    HLV_UNRUN_CLOSED=1
    local name why
    for name in $HLV_REQUIRED_CHECKS; do
        case "$HLV_RECORDED" in
            *" $name "*) ;;  # ran — a verdict exists, whatever it was
            *)
                why="required check did not run"
                [ -n "${HLV_ONLY:-}" ] && why="$why (HLV_ONLY=$HLV_ONLY)"
                hlv_record "$name" NOT-RUN "$why — a check that never ran is not a pass"
                ;;
        esac
    done
    return 0
}

# hlv_selected — honours HLV_ONLY. Returns 0 when <name> should run.
hlv_selected() {
    local name="$1"
    [ -z "${HLV_ONLY:-}" ] && return 0
    case ",${HLV_ONLY}," in
        *",$name,"*) return 0 ;;
    esac
    return 1
}

# hlv_override_lookup "<k=v> <k=v> ..." <key> — prints the value for <key>;
# returns 1 when the key is absent.
hlv_override_lookup() {
    local pair
    for pair in $1; do
        case "$pair" in
            "$2="*) printf '%s\n' "${pair#"$2="}"; return 0 ;;
        esac
    done
    return 1
}

# hlv_tcp_probe <host> <port> [timeout-s] — prints exactly one of:
#     open     the port accepted a TCP connection
#     closed   the connection was ACTIVELY refused (host answered with RST)
#     timeout  nothing answered within the deadline (filtered/unroutable/slow)
#     error    the probe itself failed (bad host, DNS failure, ...)
# Callers must treat timeout AND error as "could not determine" (fail closed).
hlv_tcp_probe() {
    local host="$1" port="$2" to="${3:-4}"
    # An empty host or port can never name a real endpoint; /dev/tcp's
    # behavior on them varies by bash version, so classify before probing.
    if [ -z "$host" ] || [ -z "$port" ]; then
        echo error
        return 0
    fi
    if [ -n "${HLV_PROBE_OVERRIDE:-}" ]; then
        local v
        if v="$(hlv_override_lookup "$HLV_PROBE_OVERRIDE" "$host:$port")"; then
            printf '%s\n' "$v"
        else
            # An override set that does not cover the asked address is a
            # harness defect; report error so scoring stays fail-closed.
            echo error
        fi
        return 0
    fi
    local errf
    errf="$(mktemp)" || { echo error; return 0; }
    ( exec 2>>"$errf"; exec 3<>"/dev/tcp/$host/$port"; exec 3>&- ) &
    local pid=$! waited=0
    while kill -0 "$pid" 2>/dev/null; do
        if [ "$waited" -ge "$to" ]; then
            kill "$pid" 2>/dev/null
            wait "$pid" 2>/dev/null
            rm -f "$errf"
            echo timeout
            return 0
        fi
        sleep 1
        waited=$((waited + 1))
    done
    wait "$pid" 2>/dev/null
    local rc=$?
    local err
    err="$(cat "$errf" 2>/dev/null)"
    rm -f "$errf"
    if [ "$rc" -eq 0 ]; then
        echo open
    elif printf '%s' "$err" | grep -qi 'refused'; then
        echo closed
    else
        echo error
    fi
}

# hlv_http_code <host> <port> [timeout-s] [path] — minimal HTTP/1.0 GET over
# /dev/tcp; prints the 3-digit status code, or prints nothing and returns 1
# when no well-formed HTTP status line came back. path defaults to /. Only
# ever pointed at 127.0.0.1 (the port-forward), where a TCP connect cannot
# hang. Offline overrides key on "host:port/path"; a path-less "host:port"
# pair is the fallback for any path.
hlv_http_code() {
    local host="$1" port="$2" to="${3:-5}" path="${4:-/}"
    if [ -n "${HLV_HTTP_OVERRIDE:-}" ]; then
        local v
        if v="$(hlv_override_lookup "$HLV_HTTP_OVERRIDE" "$host:$port$path")"; then
            [ "$v" = "none" ] && return 1
            printf '%s\n' "$v"
            return 0
        fi
        v="$(hlv_override_lookup "$HLV_HTTP_OVERRIDE" "$host:$port")" || return 1
        [ "$v" = "none" ] && return 1
        printf '%s\n' "$v"
        return 0
    fi
    local line code
    line="$(
        exec 2>/dev/null
        exec 3<>"/dev/tcp/$host/$port" || exit 1
        printf 'GET %s HTTP/1.0\r\nHost: %s\r\nConnection: close\r\n\r\n' "$path" "$host" >&3
        IFS= read -r -t "$to" l <&3 || exit 1
        printf '%s' "$l"
    )"
    code="$(printf '%s' "$line" | grep -oE 'HTTP/[0-9.]+ [0-9]{3}' | grep -oE '[0-9]{3}$' | head -1)"
    [ -n "$code" ] || return 1
    printf '%s\n' "$code"
}

# hlv_score_exposure <target-result> <control-result> <control-port>
# Scores one (address, port) exposure probe. Prints the reason; returns
#   0 PASS (proven not exposed)  /  1 FAIL (exposed)  /  2 BLOCKED (unproven).
# Precedence: an observed open target is FAIL no matter what the control said
# (exposure was observed); otherwise an unvalidated control BLOCKS — a refusal
# on an unproven host is NOT evidence of non-exposure.
hlv_score_exposure() {
    local target="$1" control="$2" cport="$3"
    if [ "$target" = "open" ]; then
        echo "port answered unauthenticated"
        return 1
    fi
    if [ "$control" != "open" ]; then
        echo "control port $cport did not answer (result=$control) — cannot prove the probe reached the right, live host; a $target target on an unproven host is not evidence of non-exposure"
        return 2
    fi
    if [ "$target" = "closed" ]; then
        echo "connection actively refused (host proven live via control port $cport)"
        return 0
    fi
    echo "probe result $target — could not determine whether the port is open"
    return 2
}

# Guard: sourcing for tests stops here.
if [ "${HLV_LIB_ONLY:-0}" = "1" ]; then
    return 0 2>/dev/null || true
fi

# ---------------------------------------------------------------------------
# Live plumbing
# ---------------------------------------------------------------------------

HLV_NAMESPACE="${HLV_NAMESPACE:-headlamp}"
HLV_NAME="${HLV_NAME:-headlamp}"
HLV_LOCAL_PORT="${HLV_LOCAL_PORT:-18466}"
HLV_SERVICE_PORT="${HLV_SERVICE_PORT:-80}"
HLV_CONTAINER_PORT="${HLV_CONTAINER_PORT:-4466}"
HLV_CONTROL_PORT="${HLV_CONTROL_PORT:-10250}"
HLV_TIMEOUT="${HLV_TIMEOUT:-30}"

# Deterministic peer kubeconfig — NEVER ambient.
# The peer admin kubeconfig is ~/.kube/outpost-control-plane/k3s.yaml.
# ~/.kube/outpost.yaml is the CLOUDBOX one — never select it here.
# An ambient KUBECONFIG, stale ~/.kube/config, or inherited env var
# must never select the cluster. If the venue cannot be determined,
# the check must FAIL, not proceed blind.
if [ -n "${KUBECONFIG:-}" ]; then
    hlv_record preflight FAIL "KUBECONFIG is set in the environment — the peer verification must use the deterministic path, never an ambient env var"
    hlv_close_unrun; hlv_summary; exit 1
fi
PEER_KUBECONFIG="${HLV_PEER_KUBECONFIG:-${HOME:-~}/.kube/outpost-control-plane/k3s.yaml}"
PEER_KUBECONFIG="$(readlink -f "$PEER_KUBECONFIG" 2>/dev/null || readlink -f "$PEER_KUBECONFIG" 2>/dev/null || printf '%s' "$PEER_KUBECONFIG")"
if [ ! -f "$PEER_KUBECONFIG" ]; then
    hlv_record preflight FAIL "peer kubeconfig not found at $PEER_KUBECONFIG — deploy the control plane first"
    hlv_close_unrun; hlv_summary; exit 1
fi

PF_PID=""
PF_LOG=""
HLV_TEMPS=""
cleanup() {
    if [ -n "$PF_PID" ]; then
        kill "$PF_PID" 2>/dev/null
        wait "$PF_PID" 2>/dev/null
    fi
    if [ -n "$PF_LOG" ] && [ -f "$PF_LOG" ]; then
        rm -f "$PF_LOG"
    fi
    if [ -n "$HLV_TEMPS" ]; then
        rm -f $HLV_TEMPS 2>/dev/null
    fi
}
trap cleanup EXIT

# hlv_start_forward — start the loopback-pinned port-forward to the Headlamp
# Service and wait up to HLV_TIMEOUT for 127.0.0.1:$HLV_LOCAL_PORT to accept
# TCP. Returns 0 with PF_PID/PF_LOG set when the forward is up, 1 otherwise.
# hlv_stop_forward tears it down again. The --address 127.0.0.1 pin is the
# load-bearing part: the forward must never bind a non-loopback interface.
hlv_start_forward() {
    PF_LOG="$(mktemp)"
    HLV_TEMPS="$HLV_TEMPS $PF_LOG"
    kubectl --kubeconfig "$PEER_KUBECONFIG" -n "$HLV_NAMESPACE" port-forward --address 127.0.0.1 \
        "svc/$HLV_NAME" "$HLV_LOCAL_PORT:$HLV_SERVICE_PORT" >"$PF_LOG" 2>&1 &
    PF_PID=$!
    local waited=0
    while [ "$waited" -lt "$HLV_TIMEOUT" ]; do
        if [ "$(hlv_tcp_probe 127.0.0.1 "$HLV_LOCAL_PORT" 2)" = "open" ]; then
            return 0
        fi
        kill -0 "$PF_PID" 2>/dev/null || break
        sleep 1
        waited=$((waited + 1))
    done
    return 1
}

hlv_stop_forward() {
    kill "$PF_PID" 2>/dev/null
    wait "$PF_PID" 2>/dev/null
    PF_PID=""
    rm -f "$PF_LOG"
}

if ! command -v kubectl >/dev/null 2>&1; then
    hlv_record preflight BLOCKED "kubectl not on PATH"
    hlv_close_unrun; hlv_summary; exit $?
fi

PRE_OUT="$(kubectl --kubeconfig "$PEER_KUBECONFIG" get nodes -o name 2>&1)"
if [ $? -ne 0 ]; then
    hlv_record preflight BLOCKED "cannot reach cluster via $PEER_KUBECONFIG: $(printf '%s' "$PRE_OUT" | head -c 200)"
    hlv_close_unrun; hlv_summary; exit $?
fi

echo "NOTE kubeconfig=${PEER_KUBECONFIG} namespace=$HLV_NAMESPACE name=$HLV_NAME local_port=$HLV_LOCAL_PORT container_port=$HLV_CONTAINER_PORT control_port=$HLV_CONTROL_PORT"
if [ -n "${HLV_PROBE_OVERRIDE:-}" ] || [ -n "${HLV_HTTP_OVERRIDE:-}" ]; then
    echo "NOTE probe overrides active (offline test mode) — results do NOT describe a real network"
fi

# One Service snapshot shared by the shape and exposure checks, so the two
# cannot disagree about what the Service looked like.
SVC_RAW="$(kubectl --kubeconfig "$PEER_KUBECONFIG" -n "$HLV_NAMESPACE" get service "$HLV_NAME" \
    -o jsonpath='{.spec.type}|{.spec.ports[*].nodePort}|{.spec.externalIPs[*]}' 2>&1)"
if [ $? -eq 0 ]; then
    SVC_STATE=ok
elif printf '%s' "$SVC_RAW" | grep -q 'NotFound'; then
    SVC_STATE=absent
else
    SVC_STATE=error
fi
SVC_TYPE=""; SVC_NODEPORTS=""; SVC_EXTIPS=""
if [ "$SVC_STATE" = "ok" ]; then
    SVC_TYPE="${SVC_RAW%%|*}"
    _rest="${SVC_RAW#*|}"
    SVC_NODEPORTS="${_rest%%|*}"
    SVC_EXTIPS="${_rest#*|}"
fi

# ---------------------------------------------------------------------------
# 1. running
# ---------------------------------------------------------------------------
RUNNING_OK=0
RUNNING_DETAIL="check not run (HLV_ONLY)"
if hlv_selected running; then
    READY_RAW="$(kubectl --kubeconfig "$PEER_KUBECONFIG" -n "$HLV_NAMESPACE" get deployment "$HLV_NAME" \
        -o jsonpath='{.status.readyReplicas}' 2>&1)"
    RC=$?
    if [ "$RC" -ne 0 ]; then
        if printf '%s' "$READY_RAW" | grep -q 'NotFound'; then
            RUNNING_DETAIL="deployment $HLV_NAMESPACE/$HLV_NAME not found — deploy Headlamp first (headlamp.Deploy, or kubectl apply of headlamp.RenderJSON output)"
            hlv_record running FAIL "$RUNNING_DETAIL"
        else
            RUNNING_DETAIL="cannot read deployment: $(echo "$READY_RAW" | head -c 200)"
            hlv_record running BLOCKED "$RUNNING_DETAIL"
        fi
    else
        READY="${READY_RAW:-0}"
        [ -z "$READY" ] && READY=0
        case "$READY" in
            *[!0-9]*)
                RUNNING_DETAIL="unparseable readyReplicas value [$READY_RAW]"
                hlv_record running BLOCKED "$RUNNING_DETAIL"
                ;;
            *)
                if [ "$READY" -ge 1 ]; then
                    RUNNING_OK=1
                    RUNNING_DETAIL="$READY ready replica(s)"
                    hlv_record running PASS "deployment $HLV_NAMESPACE/$HLV_NAME has $READY ready replica(s)"
                else
                    RUNNING_DETAIL="deployment exists but has 0 ready replicas"
                    hlv_record running FAIL "$RUNNING_DETAIL"
                fi
                ;;
        esac
    fi
fi

# ---------------------------------------------------------------------------
# 2. service-clusterip-only — the Service must offer no off-node path.
# ---------------------------------------------------------------------------
if hlv_selected service-clusterip-only; then
    case "$SVC_STATE" in
        absent)
            hlv_record service-clusterip-only FAIL "service $HLV_NAMESPACE/$HLV_NAME not found — nothing fronts the deployment; deploy first"
            ;;
        error)
            hlv_record service-clusterip-only BLOCKED "cannot read service: $(echo "$SVC_RAW" | head -c 200)"
            ;;
        ok)
            SHAPE_BAD=""
            [ "$SVC_TYPE" != "ClusterIP" ] && SHAPE_BAD="$SHAPE_BAD type=$SVC_TYPE(want ClusterIP)"
            [ -n "$SVC_NODEPORTS" ] && SHAPE_BAD="$SHAPE_BAD nodePorts=[$SVC_NODEPORTS]"
            [ -n "$SVC_EXTIPS" ] && SHAPE_BAD="$SHAPE_BAD externalIPs=[$SVC_EXTIPS]"
            if [ -z "$SHAPE_BAD" ]; then
                hlv_record service-clusterip-only PASS "type=ClusterIP, no nodePorts, no externalIPs"
            else
                hlv_record service-clusterip-only FAIL "service is exposed beyond the cluster:$SHAPE_BAD"
            fi
            ;;
    esac
fi

# ---------------------------------------------------------------------------
# 3. rbac-zero-grant — the pod ServiceAccount must appear in NO binding.
# Its powerlessness is the auth boundary: a compromised Headlamp pod holds
# only this SA's token. (Viewer-SA drift and pod-spec drift are audited by
# headlamp.Inspect; this script checks the credential the pod actually holds.)
# ---------------------------------------------------------------------------
if hlv_selected rbac-zero-grant; then
    CRB_LINES="$(kubectl --kubeconfig "$PEER_KUBECONFIG" get clusterrolebindings \
        -o jsonpath='{range .items[*]}{.metadata.name}{range .subjects[*]} {.kind}/{.namespace}/{.name}{end}{"\n"}{end}' 2>&1)"
    CRB_RC=$?
    RB_LINES="$(kubectl --kubeconfig "$PEER_KUBECONFIG" get rolebindings -A \
        -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}{range .subjects[*]} {.kind}/{.namespace}/{.name}{end}{"\n"}{end}' 2>&1)"
    RB_RC=$?
    if [ "$CRB_RC" -ne 0 ] || [ "$RB_RC" -ne 0 ]; then
        hlv_record rbac-zero-grant BLOCKED "cannot list RBAC bindings (an unaudited grant set is not a clean one): $(printf '%s %s' "$CRB_LINES" "$RB_LINES" | head -c 200)"
    else
        # Field-exact match: " ServiceAccount/<ns>/<name>" as a whole field,
        # so the pod SA never substring-matches the viewer SA's longer name.
        HITS="$(printf '%s\n%s\n' "$CRB_LINES" "$RB_LINES" | awk -v sa="ServiceAccount/$HLV_NAMESPACE/$HLV_NAME" '
            { for (i = 2; i <= NF; i++) if ($i == sa) { printf "%s ", $1; break } }')"
        if [ -n "$HITS" ]; then
            hlv_record rbac-zero-grant FAIL "bindings grant the pod SA $HLV_NAMESPACE/$HLV_NAME: ${HITS}— it must hold zero grants (token-login model)"
        else
            hlv_record rbac-zero-grant PASS "no clusterrolebinding or rolebinding references ServiceAccount $HLV_NAMESPACE/$HLV_NAME"
        fi
    fi
fi

# ---------------------------------------------------------------------------
# 4. answering — the intended operator path: a port-forward pinned to
# 127.0.0.1, then one HTTP GET. Any well-formed status line proves the UI
# answers. What that answer is WORTH is a separate question: whether an
# anonymous GET carries any cluster authority is not assumed here — the
# auth-token-required check below positively asserts it does not.
# ---------------------------------------------------------------------------
if hlv_selected answering; then
    if [ "$RUNNING_OK" != "1" ]; then
        hlv_record answering BLOCKED "no running deployment to answer ($RUNNING_DETAIL)"
    elif [ "$SVC_STATE" != "ok" ]; then
        hlv_record answering BLOCKED "service unavailable (state=$SVC_STATE) — the port-forward targets svc/$HLV_NAME"
    else
        if ! hlv_start_forward; then
            hlv_record answering BLOCKED "loopback port-forward did not open 127.0.0.1:$HLV_LOCAL_PORT within ${HLV_TIMEOUT}s: $(tail -c 200 "$PF_LOG" 2>/dev/null)"
        else
            CODE="$(hlv_http_code 127.0.0.1 "$HLV_LOCAL_PORT" 8)"
            if [ -n "$CODE" ]; then
                hlv_record answering PASS "HTTP $CODE over the loopback-pinned port-forward at 127.0.0.1:$HLV_LOCAL_PORT"
            else
                hlv_record answering FAIL "127.0.0.1:$HLV_LOCAL_PORT accepted TCP but returned no HTTP status line — the forward is up but Headlamp is not answering"
            fi
        fi
        hlv_stop_forward
    fi
fi

# ---------------------------------------------------------------------------
# 5. auth-token-required — THE positive auth assertion. The whole security
# model stands on "the UI cannot yield authority without a pasted token", so
# that claim is PROVEN, not assumed: an unauthenticated GET of Headlamp's
# apiserver-proxy route must be shown NOT to yield cluster authority.
#
# For the pinned image (ghcr.io/headlamp-k8s/headlamp:v0.27.0, deployed with
# -in-cluster) the documented signal is an unauthenticated GET of
# /clusters/main/api/v1: the in-cluster context is named "main"
# (backend/pkg/kubeconfig InClusterContextName) and /clusters/{name}/{api}
# reverse-proxies to the apiserver forwarding only the caller's Authorization
# header — the in-cluster context itself carries NO credentials. With no
# token the request therefore reaches the apiserver anonymous and the
# apiserver itself denies it: 401 (anonymous auth off) or 403 (anonymous on,
# RBAC denies system:anonymous). Either is the gate working; even a drifted
# pod SA holds zero grants (check rbac-zero-grant), so no in-cluster
# credential can turn this into a 2xx. A 2xx IS authority without a token —
# the fail this run exists to catch. Any other answer means the pinned route
# layout drifted and the gate cannot be confirmed: BLOCKED, never a silent
# pass and never a false fail.
# ---------------------------------------------------------------------------
if hlv_selected auth-token-required; then
    if [ "$RUNNING_OK" != "1" ]; then
        hlv_record auth-token-required BLOCKED "no running deployment to probe ($RUNNING_DETAIL)"
    elif [ "$SVC_STATE" != "ok" ]; then
        hlv_record auth-token-required BLOCKED "service unavailable (state=$SVC_STATE) — the port-forward targets svc/$HLV_NAME"
    else
        if ! hlv_start_forward; then
            hlv_record auth-token-required BLOCKED "loopback port-forward did not open 127.0.0.1:$HLV_LOCAL_PORT within ${HLV_TIMEOUT}s: $(tail -c 200 "$PF_LOG" 2>/dev/null)"
        else
            ACODE="$(hlv_http_code 127.0.0.1 "$HLV_LOCAL_PORT" 8 /clusters/main/api/v1)"
            case "$ACODE" in
                401|403)
                    hlv_record auth-token-required PASS "unauthenticated GET /clusters/main/api/v1 denied with HTTP $ACODE — authority enters only with a pasted token"
                    ;;
                2*)
                    hlv_record auth-token-required FAIL "unauthenticated GET /clusters/main/api/v1 returned HTTP $ACODE — the UI serves cluster authority WITHOUT a token; the token-login boundary is broken"
                    ;;
                "")
                    hlv_record auth-token-required BLOCKED "no HTTP status for the unauthenticated probe at 127.0.0.1:$HLV_LOCAL_PORT — could not determine whether the auth gate holds"
                    ;;
                *)
                    hlv_record auth-token-required BLOCKED "unexpected HTTP $ACODE for /clusters/main/api/v1 (pinned image v0.27.0 names the in-cluster context 'main') — the route layout may have drifted; the auth gate is UNCONFIRMED"
                    ;;
            esac
        fi
        hlv_stop_forward
    fi
fi

# ---------------------------------------------------------------------------
# 6. no-unauthenticated-exposure — THE negative check. Probes EVERY discovered
# node address — InternalIP, ExternalIP AND Hostname: an ExternalIP is the
# single most likely place a Headlamp would actually be exposed, so probing
# only InternalIPs would assert non-exposure on knowingly incomplete evidence
# (the same bug class as an unreadable Service port set, which BLOCKs below —
# the port set and the address set are held to the same standard). Each
# address is probed on the Headlamp container port (catches hostNetwork/
# hostPort drift) plus any nodePort the Service carries (catches type drift),
# expecting an active refusal on a host proven live by the control port.
#
# HLV_PROBE_ADDRS overrides the probe SET but never the COVERAGE: discovery
# always runs, and every discovered node must have at least one of its
# addresses in the override — an override covering 1 of N nodes proves
# nothing about the other N-1 and BLOCKs. Extra addresses (a floating VIP,
# an external name) are allowed and probed; they can only add evidence.
# ---------------------------------------------------------------------------
if hlv_selected no-unauthenticated-exposure; then
    # Per-node discovery: "name:addr,addr,;name:addr,;" — the coverage
    # cross-check needs to know WHICH addresses belong to the same node.
    NODE_ADDRS_RAW="$(kubectl --kubeconfig "$PEER_KUBECONFIG" get nodes \
        -o jsonpath='{range .items[*]}{.metadata.name}:{range .status.addresses[*]}{.address},{end}{";"}{end}' 2>/dev/null)"
    # Flatten to the deduped union of every discovered address (the default
    # probe set) and flag any node that exposes NO address at all — a node
    # with no known address is incomplete evidence, never a pass.
    ALL_ADDRS=""; ADDRLESS_NODES=""
    for entry in $(printf '%s' "$NODE_ADDRS_RAW" | tr ';' ' '); do
        [ -n "$entry" ] || continue
        node="${entry%%:*}"
        node_addrs="$(printf '%s' "${entry#*:}" | tr ',' ' ')"
        if [ -z "${node_addrs// /}" ]; then
            ADDRLESS_NODES="$ADDRLESS_NODES $node"
            continue
        fi
        for a in $node_addrs; do
            case " $ALL_ADDRS " in
                *" $a "*) ;;  # already listed under another type/node
                *) ALL_ADDRS="$ALL_ADDRS $a" ;;
            esac
        done
    done
    ALL_ADDRS="${ALL_ADDRS# }"
    # Coverage cross-check: with HLV_PROBE_ADDRS set, every discovered node
    # must have at least one of its addresses in the override.
    UNCOVERED_NODES=""
    if [ -n "${HLV_PROBE_ADDRS:-}" ]; then
        for entry in $(printf '%s' "$NODE_ADDRS_RAW" | tr ';' ' '); do
            [ -n "$entry" ] || continue
            node="${entry%%:*}"
            covered=0
            for a in $(printf '%s' "${entry#*:}" | tr ',' ' '); do
                case " ${HLV_PROBE_ADDRS} " in
                    *" $a "*) covered=1 ;;
                esac
            done
            [ "$covered" = "1" ] || UNCOVERED_NODES="$UNCOVERED_NODES $node"
        done
    fi
    if [ "$SVC_STATE" = "error" ]; then
        hlv_record no-unauthenticated-exposure BLOCKED "cannot enumerate Service ports (service read failed) — an unknown nodePort set cannot be scored as unexposed"
    elif [ -n "$UNCOVERED_NODES" ]; then
        hlv_record no-unauthenticated-exposure BLOCKED "HLV_PROBE_ADDRS leaves discovered node(s) with no probed address:$UNCOVERED_NODES — an override that does not cover every discovered node cannot prove non-exposure"
    elif [ -n "$ADDRLESS_NODES" ]; then
        hlv_record no-unauthenticated-exposure BLOCKED "node(s) expose no address to probe:$ADDRLESS_NODES — a node with no known address is incomplete evidence, never a pass"
    elif [ -n "${HLV_PROBE_ADDRS:-}" ] && [ -z "${NODE_ADDRS_RAW//;/}" ]; then
        hlv_record no-unauthenticated-exposure BLOCKED "HLV_PROBE_ADDRS is set but the cluster's node set could not be enumerated — an override that cannot be cross-checked against the real node set is not evidence"
    else
        ADDRS="${HLV_PROBE_ADDRS:-$ALL_ADDRS}"
        if [ -z "${ADDRS// /}" ]; then
            hlv_record no-unauthenticated-exposure BLOCKED "no node addresses to probe (kubectl returned none and HLV_PROBE_ADDRS is unset)"
        else
            PORTS="$HLV_CONTAINER_PORT"
            [ "$SVC_STATE" = "ok" ] && [ -n "$SVC_NODEPORTS" ] && PORTS="$PORTS $SVC_NODEPORTS"
            NE_FAIL=""; NE_BLOCKED=""; NE_OK=""
            for addr in $ADDRS; do
                CONTROL="$(hlv_tcp_probe "$addr" "$HLV_CONTROL_PORT")"
                for port in $PORTS; do
                    TARGET="$(hlv_tcp_probe "$addr" "$port")"
                    REASON="$(hlv_score_exposure "$TARGET" "$CONTROL" "$HLV_CONTROL_PORT")"
                    case $? in
                        0) NE_OK="$NE_OK $addr:$port" ;;
                        1) NE_FAIL="$NE_FAIL $addr:$port($REASON)" ;;
                        *) NE_BLOCKED="$NE_BLOCKED $addr:$port($REASON)" ;;
                    esac
                done
            done
            if [ -n "$NE_FAIL" ]; then
                hlv_record no-unauthenticated-exposure FAIL "unauthenticated reachability observed:$NE_FAIL"
            elif [ -n "$NE_BLOCKED" ]; then
                hlv_record no-unauthenticated-exposure BLOCKED "could not prove non-exposure:$NE_BLOCKED — run from a host with LAN/tailnet reach to the nodes (the control-plane host qualifies), or set HLV_PROBE_ADDRS/HLV_CONTROL_PORT (an override must cover every discovered node)"
            else
                if [ -n "${HLV_PROBE_ADDRS:-}" ]; then
                    NE_SCOPE="HLV_PROBE_ADDRS covering every discovered node"
                else
                    NE_SCOPE="all discovered node addresses, all types"
                fi
                hlv_record no-unauthenticated-exposure PASS "actively refused on:$NE_OK ($NE_SCOPE; each host proven live via control port $HLV_CONTROL_PORT)"
            fi
        fi
    fi
fi

echo
hlv_close_unrun
hlv_summary
exit $?
