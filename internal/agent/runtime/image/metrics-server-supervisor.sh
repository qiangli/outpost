#!/bin/bash
# Run metrics-server inside the peer control-plane container's network
# namespace. frps publishes every authenticated worker kubelet on this
# namespace's 127/8, so colocating the scraper preserves tunnel isolation:
# no kubelet port is opened on the host, LAN, overlay, or pod network.
set -eu

log() { echo "[metrics-server] $*" >&2; }

render_metrics_resources() {
    address="$1"
    case "$address" in *:*) address_type=IPv6 ;; *) address_type=IPv4 ;; esac
    cat <<EOF
apiVersion: v1
kind: ServiceAccount
metadata:
  name: metrics-server
  namespace: kube-system
  labels: {k8s-app: metrics-server}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: system:aggregated-metrics-reader
  labels:
    k8s-app: metrics-server
    rbac.authorization.k8s.io/aggregate-to-admin: "true"
    rbac.authorization.k8s.io/aggregate-to-edit: "true"
    rbac.authorization.k8s.io/aggregate-to-view: "true"
rules:
- apiGroups: [metrics.k8s.io]
  resources: [pods, nodes]
  verbs: [get, list, watch]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: system:metrics-server
  labels: {k8s-app: metrics-server}
rules:
- apiGroups: [""]
  resources: [nodes/metrics]
  verbs: [get]
- apiGroups: [""]
  resources: [pods, nodes]
  verbs: [get, list, watch]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: metrics-server-auth-reader
  namespace: kube-system
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: extension-apiserver-authentication-reader
subjects:
- kind: ServiceAccount
  name: metrics-server
  namespace: kube-system
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: "metrics-server:system:auth-delegator"}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: system:auth-delegator
subjects:
- kind: ServiceAccount
  name: metrics-server
  namespace: kube-system
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: "system:metrics-server"}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: system:metrics-server
subjects:
- kind: ServiceAccount
  name: metrics-server
  namespace: kube-system
---
apiVersion: v1
kind: Service
metadata:
  name: metrics-server
  namespace: kube-system
  labels: {k8s-app: metrics-server}
spec:
  ports:
  - appProtocol: https
    name: https
    port: 443
    protocol: TCP
    targetPort: 10250
---
# A selectorless endpoint points the aggregation layer at the colocated
# process. The address is private to the control-plane container network.
apiVersion: v1
kind: Endpoints
metadata:
  name: metrics-server
  namespace: kube-system
  labels: {k8s-app: metrics-server}
  annotations:
    endpointslice.kubernetes.io/skip-mirror: "true"
subsets:
- addresses:
  - ip: "$address"
  ports:
  - name: https
    port: 10250
    protocol: TCP
---
apiVersion: discovery.k8s.io/v1
kind: EndpointSlice
metadata:
  name: metrics-server-colocated
  namespace: kube-system
  labels:
    k8s-app: metrics-server
    kubernetes.io/service-name: metrics-server
addressType: $address_type
endpoints:
- addresses: ["$address"]
  conditions: {ready: true}
ports:
- name: https
  port: 10250
  protocol: TCP
---
apiVersion: apiregistration.k8s.io/v1
kind: APIService
metadata:
  name: v1beta1.metrics.k8s.io
  labels: {k8s-app: metrics-server}
spec:
  group: metrics.k8s.io
  groupPriorityMinimum: 100
  insecureSkipTLSVerify: true
  service:
    name: metrics-server
    namespace: kube-system
    port: 443
  version: v1beta1
  versionPriority: 100
EOF
}

# Tests source the renderer without generating credentials or starting a
# process. This keeps the network-boundary contract deterministic and offline.
if [ "${OUTPOST_METRICS_LIB_ONLY:-0}" = 1 ]; then
    return 0 2>/dev/null || exit 0
fi

: "${OUTPOST_METRICS_ADDRESS:?required: control-plane container address}"
: "${OUTPOST_METRICS_PORT:=10250}"
# Every absolute path and every budget is a seam. Defaults are exactly the
# in-container locations and timings; a test overrides them to run the real
# supervisor against stub binaries on a temporary PATH.
work="${OUTPOST_METRICS_WORK_DIR:-/run/outpost-metrics-server}"
internal_kubeconfig="${OUTPOST_METRICS_KUBECONFIG:-/etc/rancher/k3s/k3s-internal.yaml}"
tls_dir="${OUTPOST_METRICS_TLS_DIR:-/var/lib/rancher/k3s/server/tls}"
# The distroless uid the metrics child drops to. Empty skips the chown for a
# test running off-image, where that account does not exist.
metrics_owner="${OUTPOST_METRICS_OWNER-65532:65532}"
# Bounded wait for the apiserver's authorizer to catch up with the bindings
# applied below (tries x interval seconds).
auth_wait_tries="${OUTPOST_METRICS_AUTH_WAIT_TRIES:-60}"
auth_wait_interval="${OUTPOST_METRICS_AUTH_WAIT_INTERVAL:-2}"
# Bounded restart budget for a metrics-server that exits on a forbidden error.
retry_budget="${OUTPOST_METRICS_RETRY_BUDGET:-10}"
retry_backoff="${OUTPOST_METRICS_RETRY_BACKOFF:-2}"
retry_backoff_max="${OUTPOST_METRICS_RETRY_BACKOFF_MAX:-30}"
# Liveness poll interval and the rotation deadline measured in those ticks
# (8640 x 10s = 24h).
tick_seconds="${OUTPOST_METRICS_TICK_SECONDS:-10}"
rotate_ticks="${OUTPOST_METRICS_ROTATE_TICKS:-8640}"
mkdir -p "$work/certs"
chmod 0700 "$work" "$work/certs"
metrics_log="$work/metrics-server.log"

client_ca="$tls_dir/client-ca.crt"
client_key="$tls_dir/client-ca.key"
server_ca="$tls_dir/server-ca.crt"
for f in "$client_ca" "$client_key" "$server_ca"; do
    [ -s "$f" ] || { log "FATAL: required k3s credential missing: $f"; exit 1; }
done

# Mint a short-lived, least-privilege client certificate for the canonical
# ServiceAccount identity. The RBAC below grants only metrics-server's stock
# reads and delegated auth checks; the process never receives system:admin.
# The supervisor rotates this 48-hour certificate every 24 hours.
mint_credentials() {
    rm -f "$work/client.key" "$work/client.crt" "$work/kubeconfig"
    outpost certgen metrics --key "$work/client.key" --cert "$work/client.crt" \
        --ca "$client_ca" --ca-key "$client_key"

    ca_data="$(base64 < "$server_ca" | tr -d '\n')"
    cert_data="$(base64 < "$work/client.crt" | tr -d '\n')"
    key_data="$(base64 < "$work/client.key" | tr -d '\n')"
    cat > "$work/kubeconfig" <<EOF
apiVersion: v1
kind: Config
clusters:
- name: local
  cluster:
    server: https://127.0.0.1:${OUTPOST_API_PORT:-6443}
    certificate-authority-data: $ca_data
users:
- name: metrics-server
  user:
    client-certificate-data: $cert_data
    client-key-data: $key_data
contexts:
- name: local
  context: {cluster: local, user: metrics-server}
current-context: local
EOF
    chmod 0600 "$work/kubeconfig"
    [ -z "$metrics_owner" ] || chown -R "$metrics_owner" "$work"
}

# The apiserver's authorizer caches RBAC: it can accept the bindings applied
# below and still answer 403 for the metrics-server identity for a moment
# afterwards. metrics-server treats that 403 as fatal and exits, so ask the
# authorizer directly, with a bounded budget, before handing it the process.
metrics_rbac_ready() {
    [ "$(kubectl --kubeconfig="$internal_kubeconfig" auth can-i get \
        configmaps/extension-apiserver-authentication -n kube-system \
        --as=system:serviceaccount:kube-system:metrics-server 2>/dev/null || true)" = yes ]
}

wait_for_metrics_rbac() {
    waited=0
    while ! metrics_rbac_ready; do
        waited=$((waited + 1))
        if [ "$waited" -ge "$auth_wait_tries" ]; then
            log "WARNING: authorizer still refuses the metrics-server identity after ${waited} checks; starting anyway"
            return 0
        fi
        sleep "$auth_wait_interval"
    done
    [ "$waited" -eq 0 ] || log "authorizer admitted the metrics-server identity after ${waited} checks"
}

# A forbidden exit is the bootstrap race, not a broken deployment: retry it.
# Both spellings the apiserver uses ("is forbidden", "403 Forbidden") match
# without depending on a grep -i the scratch image need not provide.
metrics_exit_was_forbidden() {
    case "$(tail -n 200 "$metrics_log" 2>/dev/null || true)" in
        *orbidden*) return 0 ;;
    esac
    return 1
}

render_metrics_resources "$OUTPOST_METRICS_ADDRESS" > "$work/resources.yaml"

# Upgrade the failed headless prototype in place. APIService's resolver
# refuses ClusterIP=None, and Service clusterIP is immutable, so a one-time
# exact-name delete is required before the corrected ordinary Service can be
# allocated. No workloads or data hang from this Service.
old_cluster_ip="$(kubectl --kubeconfig="$internal_kubeconfig" \
    get service metrics-server -n kube-system \
    -o jsonpath='{.spec.clusterIP}' 2>/dev/null || true)"
if [ "$old_cluster_ip" = "None" ]; then
    log "migrating headless metrics Service to a private ClusterIP"
    kubectl --kubeconfig="$internal_kubeconfig" \
        delete service metrics-server -n kube-system --wait=true >/dev/null 2>&1 || true
fi

# The apiserver may have opened its socket before admission/RBAC are ready.
# Retry for two minutes, then fail the whole control-plane container loudly.
i=0
until apply_error="$(kubectl --kubeconfig="$internal_kubeconfig" apply --validate=false -f "$work/resources.yaml" 2>&1)"; do
    i=$((i + 1))
    [ "$i" -lt 120 ] || { log "FATAL: could not register metrics API: $apply_error"; exit 1; }
    sleep 1
done

# The aggregation layer insists on routing APIService traffic through an
# ordinary Service ClusterIP. This control plane intentionally has no local
# k3s agent/kube-proxy, so make ONLY that allocated /32 local and bridge its
# TLS port to the unprivileged sibling process. The address exists solely in
# this container namespace; no host/LAN port is published.
service_ip="$(kubectl --kubeconfig="$internal_kubeconfig" \
    get service metrics-server -n kube-system \
    -o jsonpath='{.spec.clusterIP}')"
[ -n "$service_ip" ] && [ "$service_ip" != "None" ] || {
    log "FATAL: metrics Service has no ordinary ClusterIP"
    exit 1
}
outpost netaddr add "$service_ip/32" dev lo
log "bridging private Service ${service_ip}:443 to ${OUTPOST_METRICS_ADDRESS}:${OUTPOST_METRICS_PORT}"
outpost tlsproxy --listen "${service_ip}:443" \
    --forward "${OUTPOST_METRICS_ADDRESS}:${OUTPOST_METRICS_PORT}" \
    >"$work/service-proxy.log" 2>&1 &
service_proxy_pid=$!

# Keep root only in this tiny supervisor so it can renew the client identity;
# every network-facing metrics-server child drops to the distroless uid/gid.
metrics_pid=""
stop_metrics() {
    if [ -n "$metrics_pid" ] && kill -0 "$metrics_pid" 2>/dev/null; then
        kill -TERM "$metrics_pid" 2>/dev/null || true
        wait "$metrics_pid" 2>/dev/null || true
    fi
    metrics_pid=""
}
stop_service_proxy() {
    if [ -n "${service_proxy_pid:-}" ] && kill -0 "$service_proxy_pid" 2>/dev/null; then
        kill -TERM "$service_proxy_pid" 2>/dev/null || true
        wait "$service_proxy_pid" 2>/dev/null || true
    fi
}
trap 'stop_metrics; stop_service_proxy; exit 0' TERM INT

restarts=0
backoff="$retry_backoff"
while :; do
    mint_credentials
    wait_for_metrics_rbac
    log "serving on ${OUTPOST_METRICS_ADDRESS}:${OUTPOST_METRICS_PORT}"
    # Captured rather than streamed so the supervisor can classify an early
    # exit; the tail is echoed to the container log whenever the child dies.
    : > "$metrics_log"
    # Virtual runtimes implement Pod lifecycle, not the kubelet resource
    # metrics endpoint. Scraping them can never succeed and otherwise wakes
    # the control plane every metric-resolution interval just to log "no
    # address matched types [ExternalIP]". The != selector includes unlabeled
    # legacy real agents while excluding the canonical virtual runtime label.
    outpost metrics-exec \
        --bind-address="$OUTPOST_METRICS_ADDRESS" \
        --secure-port="$OUTPOST_METRICS_PORT" \
        --cert-dir="$work/certs" \
        --kubeconfig="$work/kubeconfig" \
        --authentication-kubeconfig="$work/kubeconfig" \
        --authorization-kubeconfig="$work/kubeconfig" \
        --node-selector='outpost.dhnt.io/runtime!=virtual' \
        --kubelet-preferred-address-types=ExternalIP \
        --kubelet-use-node-status-port \
        --kubelet-insecure-tls \
        --metric-resolution=15s >>"$metrics_log" 2>&1 &
    metrics_pid=$!

    # Rotate after 24h. Polling also notices an unexpected child exit within
    # ten seconds and fails the control-plane container instead of going green
    # with a dead Metrics API.
    ticks=0
    while [ "$ticks" -lt "$rotate_ticks" ] && kill -0 "$metrics_pid" 2>/dev/null; do
        sleep "$tick_seconds"
        ticks=$((ticks + 1))
    done
    if ! kill -0 "$metrics_pid" 2>/dev/null; then
        rc=0
        wait "$metrics_pid" || rc=$?
        metrics_pid=""
        tail -n 20 "$metrics_log" >&2 || true
        # Only the RBAC race is survivable. Any other failure still takes the
        # control-plane container down on the first exit, as it always has.
        if metrics_exit_was_forbidden && [ "$restarts" -lt "$retry_budget" ]; then
            restarts=$((restarts + 1))
            log "metrics-server exited (${rc}) on a forbidden error; retry ${restarts}/${retry_budget} in ${backoff}s"
            sleep "$backoff"
            if [ "$backoff" -lt "$retry_backoff_max" ]; then
                backoff=$((backoff * 2))
                [ "$backoff" -le "$retry_backoff_max" ] || backoff="$retry_backoff_max"
            fi
            continue
        fi
        log "FATAL: metrics-server exited unexpectedly (${rc})"
        exit 1
    fi
    restarts=0
    backoff="$retry_backoff"
    log "rotating metrics-server client certificate"
    stop_metrics
done
