#!/bin/sh
# karmada-publisher.sh — hold the Karmada apiserver on 127.0.0.1:<port> inside
# the control-plane container so frpc's stcp "karmada-apiserver" proxy has
# something to dial (docs/dks-federation-karmada.md 5.3).
#
# `kubectl port-forward` rides apiserver -> frps-published kubelet, the same
# hop `kubectl logs` uses, so it works whichever worker the pod lands on and
# needs nothing on the worker. It exits whenever the pod moves or restarts, so
# this loops forever with 2s -> 30s backoff; whatever binds a port retries.
#
# It MUST read k3s-internal.yaml: the in-container k3s.yaml is rewritten to the
# host port (16443) and cannot be dialled from here.
#
#   OUTPOST_FEDERATION_PORT   loopback port to hold (default 15443)
#   OUTPOST_FEDERATION_NS     namespace (default karmada-system)
#   OUTPOST_FEDERATION_SVC    service (default karmada-apiserver)
#   OUTPOST_FEDERATION_SVC_PORT  service port (default 5443)
#   OUTPOST_PUBLISHER_KUBECTL command to run (default kubectl, the k3s argv0 symlink)
set -u

PORT="${OUTPOST_FEDERATION_PORT:-15443}"
NS="${OUTPOST_FEDERATION_NS:-karmada-system}"
SVC="${OUTPOST_FEDERATION_SVC:-karmada-apiserver}"
SVC_PORT="${OUTPOST_FEDERATION_SVC_PORT:-5443}"
KUBECONFIG_FILE="${OUTPOST_PUBLISHER_KUBECONFIG:-/etc/rancher/k3s/k3s-internal.yaml}"
KUBECTL="${OUTPOST_PUBLISHER_KUBECTL:-kubectl}"
MIN_BACKOFF="${OUTPOST_PUBLISHER_MIN_BACKOFF:-2}"
MAX_BACKOFF="${OUTPOST_PUBLISHER_MAX_BACKOFF:-30}"
ONCE="${OUTPOST_PUBLISHER_ONCE:-0}"

log() { echo "karmada-publisher: $*"; }

# Pure helper so the schedule is unit-testable: next = min(cur*2, max).
next_backoff() {
    n=$(($1 * 2))
    if [ "$n" -gt "$MAX_BACKOFF" ]; then n="$MAX_BACKOFF"; fi
    echo "$n"
}

[ -n "${OUTPOST_PUBLISHER_LIB_ONLY:-}" ] && return 0 2>/dev/null

backoff="$MIN_BACKOFF"
while :; do
    started=$(date +%s)
    # The Service appears only once the operator has built the control plane.
    if $KUBECTL --kubeconfig "$KUBECONFIG_FILE" -n "$NS" get "svc/$SVC" >/dev/null 2>&1; then
        log "forwarding svc/$SVC:$SVC_PORT -> 127.0.0.1:$PORT"
        $KUBECTL --kubeconfig "$KUBECONFIG_FILE" --address 127.0.0.1 -n "$NS" \
            port-forward "svc/$SVC" "$PORT:$SVC_PORT" || true
        # A forward that held for a while earns a fresh backoff.
        if [ $(($(date +%s) - started)) -ge 60 ]; then backoff="$MIN_BACKOFF"; fi
        log "port-forward exited; retrying in ${backoff}s"
    else
        log "svc/$SVC not present yet in $NS; retrying in ${backoff}s"
    fi
    [ "$ONCE" = "1" ] && exit 0
    sleep "$backoff"
    backoff=$(next_backoff "$backoff")
done
