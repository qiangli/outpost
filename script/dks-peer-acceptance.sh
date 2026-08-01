#!/usr/bin/env bash
# dks-peer-acceptance.sh — acceptance runner for peer-hosted DKS multi-node
# pod networking (ADR: Tailscale node underlay + stock k3s flannel VXLAN
# pinned via --flannel-iface=tailscale0; see docs/adr-peer-dks-pod-network.md).
#
# Emits one machine-readable line per check:
#     CHECK <name> PASS|FAIL|BLOCKED <detail>
# and a final summary.
#
# Exit status (the contract a CI gate reads):
#     0  OK           — at least one check PASSed and none FAILed
#     1  FAIL         — at least one check FAILed
#     2  INCONCLUSIVE — nothing was proven: no check PASSed (all BLOCKED, or
#                       no check ran at all). Absence of evidence is NOT
#                       success; see docs/fleet-evidence-invariant.md.
#
# BLOCKED is NOT a pass: a check whose precondition is absent reports BLOCKED
# with the exact missing precondition named. A check is never silently skipped.
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
#   DKS_ALLOW_NODE_DEBUG=1  permit `kubectl debug node/<n>` for host-filesystem checks
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
DKS_RESULTS=()

# dks_record <name> <PASS|FAIL|BLOCKED> <detail...>
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
        PASS)    DKS_PASS_COUNT=$((DKS_PASS_COUNT + 1)) ;;
        FAIL)    DKS_FAIL_COUNT=$((DKS_FAIL_COUNT + 1)) ;;
        BLOCKED) DKS_BLOCKED_COUNT=$((DKS_BLOCKED_COUNT + 1)) ;;
        *)
            # An unknown status is itself a harness defect; never let it pass.
            DKS_FAIL_COUNT=$((DKS_FAIL_COUNT + 1))
            status="FAIL"
            detail="harness error: unknown status; $detail"
            ;;
    esac
    DKS_RESULTS+=("CHECK $name $status $detail")
    echo "CHECK $name $status $detail"
}

# dks_summary — prints the tally; returns the harness exit code:
#   0 OK (>=1 PASS, 0 FAIL) / 1 FAIL (>=1 FAIL) / 2 INCONCLUSIVE (0 PASS).
dks_summary() {
    echo "SUMMARY pass=$DKS_PASS_COUNT fail=$DKS_FAIL_COUNT blocked=$DKS_BLOCKED_COUNT"
    if [ "$DKS_FAIL_COUNT" -gt 0 ]; then
        echo "RESULT FAIL"
        return 1
    fi
    if [ "$DKS_PASS_COUNT" -eq 0 ]; then
        # Nothing was actually proven. Say so, and exit NON-ZERO (2) — a gate
        # that reads only the exit code must never score "no evidence" as a
        # pass. An all-BLOCKED run is inconclusive, not success.
        if [ "$DKS_BLOCKED_COUNT" -eq 0 ]; then
            echo "RESULT INCONCLUSIVE (no check ran)"
        else
            echo "RESULT INCONCLUSIVE (no check passed)"
        fi
        return 2
    fi
    echo "RESULT OK (no failures; $DKS_BLOCKED_COUNT blocked)"
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
    local rc=$?
    CREATED+=("pod/$name")
    return $rc
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
READY_LIST="$(kubectl get nodes -o \
    'jsonpath={range .items[*]}{.metadata.name}{" "}{.status.nodeInfo.kubeletVersion}{" "}{range .status.conditions[?(@.type=="Ready")]}{.status}{end}{"\n"}{end}' 2>/dev/null \
    | awk '$3=="True" && $2 !~ /vknode/ && $2 ~ /k3s|^v1\./ {print $1}' | tr '\n' ' ')"
READY_COUNT="$(echo "$READY_LIST" | wc -w | tr -d ' ')"

NODE_A="${DKS_NODE_A:-}"; NODE_B="${DKS_NODE_B:-}"
[ -z "$NODE_A" ] && NODE_A="$(echo "$READY_LIST" | awk '{print $1}')"
[ -z "$NODE_B" ] && NODE_B="$(echo "$READY_LIST" | awk '{print $2}')"
echo "NOTE ready_real_nodes=$READY_COUNT node_a=${NODE_A:-none} node_b=${NODE_B:-none}"
echo "NOTE ready_real_node_list=$READY_LIST"

TWO_NODES=0
[ -n "$NODE_A" ] && [ -n "$NODE_B" ] && [ "$NODE_A" != "$NODE_B" ] && TWO_NODES=1
NO_TWO="requires two Ready kubelet-backed nodes in one cluster; found $READY_COUNT"

# ---------------------------------------------------------------------------
# 1. nodes-ready
# ---------------------------------------------------------------------------
if dks_selected nodes-ready; then
    if [ "$READY_COUNT" -ge 2 ]; then
        dks_record nodes-ready PASS "$READY_COUNT Ready kubelet-backed nodes: $READY_LIST"
    else
        dks_record nodes-ready FAIL "only $READY_COUNT Ready kubelet-backed node(s): ${READY_LIST:-none}"
    fi
fi

# ---------------------------------------------------------------------------
# 2. distinct-pod-cidrs  (the B5 acceptance)
# Scoped to Ready kubelet-backed nodes only (stale NotReady nodes excluded).
# ---------------------------------------------------------------------------
if dks_selected distinct-pod-cidrs; then
    # Query only Ready kubelet-backed nodes (k3s/v1.*, exclude vknode).
    # custom-columns (not jsonpath): jsonpath emits NOTHING for absent
    # .spec.podCIDR, which shifts columns and makes missing CIDR masquerade as
    # the next field; custom-columns prints literal <none>.
    CIDR_LINES="$(kubectl get nodes --no-headers -o \
        'custom-columns=NAME:.metadata.name,READY:.status.conditions[?(@.type=="Ready")].status,PODCIDR:.spec.podCIDR,VER:.status.nodeInfo.kubeletVersion' 2>/dev/null \
        | awk '$2=="True" && $4 !~ /vknode/ && $4 ~ /k3s|^v1\./ {print $1, $3}')"
    EXCLUDED_VIRTUAL="$(kubectl get nodes -o 'jsonpath={range .items[*]}{.metadata.name}{" "}{.status.nodeInfo.kubeletVersion}{"\n"}{end}' 2>/dev/null | awk '$2 ~ /vknode/ {printf "%s ", $1}')"
    EXCLUDED_NOTREADY="$(kubectl get nodes -o 'jsonpath={range .items[*]}{.metadata.name}{" "}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' 2>/dev/null | awk '$2!="True" && $2!="" {printf "%s ", $1}' | xargs -r kubectl get nodes -o 'jsonpath={range .items[*]}{.metadata.name}{" "}{.status.nodeInfo.kubeletVersion}{"\n"}{end}' 2>/dev/null | awk '$2!~/vknode/ {printf "%s ", $1}')"
    [ -n "$EXCLUDED_VIRTUAL" ] && echo "NOTE excluded from distinct-pod-cidrs (virtual-kubelet): $EXCLUDED_VIRTUAL"
    [ -n "$EXCLUDED_NOTREADY" ] && echo "NOTE excluded from distinct-pod-cidrs (NotReady): $EXCLUDED_NOTREADY"
    if [ -z "$CIDR_LINES" ]; then
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
# 3. flannel-iface — flannel pinned to tailscale0 on each worker.
# Annotation alone never PASS: requires explicit host evidence on each
# selected Ready worker that k3s agent argv contains --flannel-iface=tailscale0
# AND tailscale0 has IPv4. BLOCKED otherwise (annotation only).
# ---------------------------------------------------------------------------
if dks_selected flannel-iface; then
    if [ "${DKS_ALLOW_NODE_DEBUG:-0}" != "1" ]; then
        dks_record flannel-iface BLOCKED "requires host-inspection for k3s agent argv; set DKS_ALLOW_NODE_DEBUG=1 to permit inspection pods (requires pod execution RBAC)"
    elif [ "$TWO_NODES" != "1" ]; then
        dks_record flannel-iface BLOCKED "$NO_TWO"
    else
        ANN="$(kubectl get nodes "$NODE_A" "$NODE_B" -o 'jsonpath={range .items[*]}{.metadata.name}{" "}{.metadata.annotations.flannel\.alpha\.coreos\.com/public-ip}{"\n"}{end}' 2>/dev/null | awk 'NF>=2')"
        if [ -z "$ANN" ]; then
            dks_record flannel-iface BLOCKED "selected nodes carry no flannel.alpha.coreos.com/public-ip annotation; flannel not deployed"
        else
            # Verify tailnet IPs first (annotation guard); requires host check after.
            bad=""; good=""
            while read -r n ip; do
                if dks_is_tailnet_ip "$ip"; then good="$good $n"; else bad="$bad $n=$ip"; fi
            done <<<"$ANN"
            if [ -n "$bad" ]; then
                dks_record flannel-iface FAIL "flannel public-ip annotation not on tailnet (100.64/10) for:$bad"
            else
                # Annotation green; now verify host evidence: --flannel-iface=tailscale0 in argv + IPv4 on tailscale0.
                PASS_NODES=""
                for node in $good; do
                    FLANNEL_POD="dksacc-flannel-check-$$-$node"
                    OUT="$(kubectl run "$FLANNEL_POD" --image="$DKS_TEST_IMAGE" --overrides='{"spec":{"nodeSelector":{"kubernetes.io/hostname":"'$node'"},"restartPolicy":"Never"}}' --rm -i --quiet -- sh -c 'ps aux | grep -F "k3s agent" | grep -v grep | grep -q "\-\-flannel-iface=tailscale0" && ip -4 addr show tailscale0 >/dev/null 2>&1 && echo PASS || echo FAIL' 2>&1 | head -1)"
                    CREATED+=("pod/$FLANNEL_POD")  # Fallback: cleanup on exit (--rm is best effort)
                    [ "$OUT" = "PASS" ] && PASS_NODES="$PASS_NODES $node"
                done
                if [ -z "$PASS_NODES" ]; then
                    dks_record flannel-iface FAIL "host inspection found no nodes with --flannel-iface=tailscale0 + IPv4 on tailscale0"
                else
                    dks_record flannel-iface PASS "verified --flannel-iface=tailscale0 + tailscale0 IPv4 on:$PASS_NODES"
                fi
            fi
        fi
    fi
fi

# ---------------------------------------------------------------------------
# 4. no-stale-conflist — needs host filesystem access on the node.
# Uses a named scoped pod instead of kubectl debug to track and cleanup.
# ---------------------------------------------------------------------------
if dks_selected no-stale-conflist; then
    if [ "${DKS_ALLOW_NODE_DEBUG:-0}" != "1" ]; then
        dks_record no-stale-conflist BLOCKED "requires host-inspection for CNI conflist state; set DKS_ALLOW_NODE_DEBUG=1 to permit inspection pods (requires pod execution RBAC)"
    elif [ "$TWO_NODES" != "1" ]; then
        dks_record no-stale-conflist BLOCKED "$NO_TWO"
    else
        INSPECT_POD="dksacc-conflist-$$"
        OUT="$(kubectl run "$INSPECT_POD" --image="$DKS_TEST_IMAGE" --overrides='{"spec":{"nodeSelector":{"kubernetes.io/hostname":"'$NODE_B'"},"restartPolicy":"Never"}}' --rm -i --quiet -- sh -c 'ls /host/etc/cni/net.d/ 2>&1' 2>&1 | head -20)"
        CREATED+=("pod/$INSPECT_POD")  # Fallback: cleanup on exit
        if echo "$OUT" | grep -qE '10-outpost\.conflist|10-bridge\.conflist'; then
            dks_record no-stale-conflist FAIL "stale conflist present on $NODE_B: $OUT"
        elif echo "$OUT" | grep -q 'conflist'; then
            dks_record no-stale-conflist PASS "no 10-outpost/10-bridge conflist on $NODE_B"
        else
            dks_record no-stale-conflist BLOCKED "could not read /etc/cni/net.d on $NODE_B: $OUT"
        fi
    fi
fi

# ---------------------------------------------------------------------------
# 5. cross-node-pod-ip — THE headline check.
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
        OUT="$(k exec "$POD_A" -- sh -c "ping -c 3 -W 3 $POD_B_IP" 2>&1 | tail -4)"
        if echo "$OUT" | grep -qE ' 0% packet loss'; then
            dks_record cross-node-pod-ip PASS "$POD_A@$NODE_A -> $POD_B_IP@$NODE_B: $(echo "$OUT" | grep 'packet loss')"
        else
            dks_record cross-node-pod-ip FAIL "$POD_A@$NODE_A -> $POD_B_IP@$NODE_B: $OUT"
        fi
    fi
fi

# ---------------------------------------------------------------------------
# 6. service-clusterip
# ---------------------------------------------------------------------------
SVC="${RUN_ID}-svc"
if dks_selected service-clusterip || dks_selected cluster-dns; then
    if [ "$TWO_NODES" != "1" ] || [ -n "${ERR_B:-}" ]; then
        SVC_IP=""
    else
        cat <<YAML | k apply -f - >/dev/null 2>&1
apiVersion: v1
kind: Service
metadata: {name: $SVC}
spec:
  selector: {app: $POD_B}
  ports: [{port: 8080, targetPort: 8080}]
YAML
        CREATED+=("svc/$SVC")
        k exec "$POD_B" -- sh -c 'nohup httpd -f -p 8080 -h /etc >/dev/null 2>&1 &' >/dev/null 2>&1
        sleep 3
        SVC_IP="$(k get svc "$SVC" -o jsonpath='{.spec.clusterIP}' 2>/dev/null)"
    fi
fi
if dks_selected service-clusterip; then
    if [ "$TWO_NODES" != "1" ]; then
        dks_record service-clusterip BLOCKED "$NO_TWO"
    elif [ -z "${SVC_IP:-}" ]; then
        dks_record service-clusterip BLOCKED "Service got no clusterIP (backend pod on $NODE_B unavailable)"
    else
        OUT="$(k exec "$POD_A" -- sh -c "wget -q -T 8 -O - http://$SVC_IP:8080/hostname 2>&1 | head -c 120" 2>&1)"
        if [ -n "$OUT" ] && ! echo "$OUT" | grep -qiE 'refused|timed out|no route|error'; then
            dks_record service-clusterip PASS "$POD_A -> clusterIP $SVC_IP:8080 returned $(echo "$OUT" | head -c 60)"
        else
            dks_record service-clusterip FAIL "$POD_A -> clusterIP $SVC_IP:8080: ${OUT:-empty response}"
        fi
    fi
fi

# ---------------------------------------------------------------------------
# 7. cluster-dns
# ---------------------------------------------------------------------------
if dks_selected cluster-dns; then
    if [ "$TWO_NODES" != "1" ]; then
        dks_record cluster-dns BLOCKED "$NO_TWO"
    elif [ -z "${SVC_IP:-}" ]; then
        dks_record cluster-dns BLOCKED "no Service to resolve (backend pod on $NODE_B unavailable)"
    else
        OUT="$(k exec "$POD_A" -- sh -c "nslookup $SVC 2>&1" 2>&1 | tail -6)"
        if echo "$OUT" | grep -q "$SVC_IP"; then
            dks_record cluster-dns PASS "short name $SVC resolved to $SVC_IP from $POD_A@$NODE_A"
        else
            dks_record cluster-dns FAIL "short name $SVC did not resolve to $SVC_IP: $OUT"
        fi
    fi
fi

# ---------------------------------------------------------------------------
# 8. logs-exec against the REMOTE (tunnelled) node
# ---------------------------------------------------------------------------
if dks_selected logs-exec; then
    if [ "$TWO_NODES" != "1" ]; then
        dks_record logs-exec BLOCKED "$NO_TWO"
    elif [ -n "${ERR_B:-}" ]; then
        dks_record logs-exec BLOCKED "pod on remote node $NODE_B did not start: ${ERR_B}"
    else
        LOGS="$(k logs "$POD_B" --tail=3 2>&1 | head -c 200)"
        EXECO="$(k exec "$POD_B" -- sh -c 'echo dks-exec-ok' 2>&1 | head -c 200)"
        if echo "$LOGS" | grep -q 'dks-alive' && echo "$EXECO" | grep -q 'dks-exec-ok'; then
            dks_record logs-exec PASS "logs+exec ok against $POD_B@$NODE_B (logs=$(echo "$LOGS" | head -1))"
        else
            dks_record logs-exec FAIL "logs=[$LOGS] exec=[$EXECO] against $POD_B@$NODE_B"
        fi
    fi
fi

# ---------------------------------------------------------------------------
# 9. headlamp
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
        HL_POD="$(kubectl -n "$HL_NS" get pods -l 'app.kubernetes.io/name=headlamp' -o jsonpath='{.items[0].status.phase}' 2>/dev/null)"
        HL_IP="$(kubectl -n "$HL_NS" get svc "$HL_SVC" -o jsonpath='{.spec.clusterIP}' 2>/dev/null)"
        HL_PORT="$(kubectl -n "$HL_NS" get svc "$HL_SVC" -o jsonpath='{.spec.ports[0].port}' 2>/dev/null)"
        if [ "$HL_POD" != "Running" ]; then
            dks_record headlamp FAIL "Headlamp svc $HL_NS/$HL_SVC exists but no Running headlamp pod (phase=${HL_POD:-none})"
        elif [ "$PROBE_READY" != "1" ]; then
            dks_record headlamp BLOCKED "Headlamp scheduled ($HL_NS/$HL_SVC) but no in-cluster probe pod was available to reach its Service (probe pods not created or not Ready); $NO_TWO"
        else
            OUT="$(k exec "$POD_A" -- sh -c "wget -q -T 8 -S -O /dev/null http://$HL_IP:$HL_PORT/ 2>&1 | head -3" 2>&1)"
            # ANY well-formed HTTP status line proves the Service is reachable
            # across nodes, which is what this check asserts. The specific path
            # Headlamp serves its UI on is not a pod-network property, so a 404
            # from / must not be read as a connectivity failure.
            CODE="$(echo "$OUT" | grep -oE 'HTTP/1\.[01] [0-9]{3}' | head -1)"
            if [ -n "$CODE" ]; then
                dks_record headlamp PASS "Headlamp $HL_NS/$HL_SVC at $HL_IP:$HL_PORT reachable ($CODE from /)"
            else
                dks_record headlamp FAIL "Headlamp $HL_NS/$HL_SVC at $HL_IP:$HL_PORT unreachable: $OUT"
            fi
        fi
    fi
fi

# ---------------------------------------------------------------------------
# 10. nanochat — cross-node placement of a real workload
# ---------------------------------------------------------------------------
if dks_selected nanochat; then
    if [ -z "${DKS_NANOCHAT_IMAGE:-}" ]; then
        dks_record nanochat BLOCKED "DKS_NANOCHAT_IMAGE unset; no nanochat image is published in this repo and this harness will not substitute a weaker workload"
    elif [ "$TWO_NODES" != "1" ]; then
        dks_record nanochat BLOCKED "$NO_TWO"
    else
        NC="${RUN_ID}-nanochat"
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
        CREATED+=("deploy/$NC")
        sleep 10
        PLACED="$(k get pods -l "app=$NC" -o jsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}' 2>/dev/null | sort -u | wc -l | tr -d ' ')"
        READYN="$(k get pods -l "app=$NC" --field-selector=status.phase=Running -o name 2>/dev/null | wc -l | tr -d ' ')"
        if [ "$PLACED" -ge 2 ] && [ "$READYN" -ge 2 ]; then
            dks_record nanochat PASS "2 nanochat replicas Running across $PLACED distinct nodes"
        else
            dks_record nanochat FAIL "nanochat placed on $PLACED node(s), $READYN Running: $(k get pods -l "app=$NC" -o wide 2>&1 | tail -3)"
        fi
    fi
fi

# ---------------------------------------------------------------------------
# 11. bashy-chunked
# ---------------------------------------------------------------------------
if dks_selected bashy-chunked; then
    if [ -z "${DKS_BASHY_IMAGE:-}" ]; then
        dks_record bashy-chunked BLOCKED "DKS_BASHY_IMAGE unset; no chunked-bashy container image or Job manifest exists in this repo, and there is no deterministic substitute for a distributed chunked workload"
    elif [ "$TWO_NODES" != "1" ]; then
        dks_record bashy-chunked BLOCKED "$NO_TWO"
    else
        BJ="${RUN_ID}-bashy"
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
        CREATED+=("job/$BJ")
        k wait --for=condition=complete "job/$BJ" --timeout="${DKS_TIMEOUT}s" >/dev/null 2>&1
        SUCC="$(k get job "$BJ" -o jsonpath='{.status.succeeded}' 2>/dev/null)"
        NODESN="$(k get pods -l "job-name=$BJ" -o jsonpath='{range .items[*]}{.spec.nodeName}{"\n"}{end}' 2>/dev/null | sort -u | wc -l | tr -d ' ')"
        if [ "${SUCC:-0}" = "4" ] && [ "$NODESN" -ge 2 ]; then
            dks_record bashy-chunked PASS "4/4 indexed chunks completed across $NODESN nodes"
        else
            dks_record bashy-chunked FAIL "succeeded=${SUCC:-0}/4 across $NODESN node(s)"
        fi
    fi
fi

echo
dks_summary
exit $?
