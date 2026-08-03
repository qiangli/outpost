#!/bin/sh
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
work=/run/outpost-metrics-server
internal_kubeconfig=/etc/rancher/k3s/k3s-internal.yaml
mkdir -p "$work/certs"
chmod 0700 "$work" "$work/certs"

client_ca=/var/lib/rancher/k3s/server/tls/client-ca.crt
client_key=/var/lib/rancher/k3s/server/tls/client-ca.key
server_ca=/var/lib/rancher/k3s/server/tls/server-ca.crt
for f in "$client_ca" "$client_key" "$server_ca"; do
    [ -s "$f" ] || { log "FATAL: required k3s credential missing: $f"; exit 1; }
done

# Mint a short-lived, least-privilege client certificate for the canonical
# ServiceAccount identity. The RBAC below grants only metrics-server's stock
# reads and delegated auth checks; the process never receives system:admin.
# The supervisor rotates this 48-hour certificate every 24 hours.
mint_credentials() {
    rm -f "$work/client.key" "$work/client.csr" "$work/client.crt" "$work/client.ext" "$work/kubeconfig"
    openssl req -new -newkey rsa:2048 -nodes \
        -keyout "$work/client.key" -out "$work/client.csr" \
        -subj '/CN=system:serviceaccount:kube-system:metrics-server/O=system:serviceaccounts/O=system:serviceaccounts:kube-system/O=system:authenticated' \
        >/dev/null 2>&1
    cat > "$work/client.ext" <<'EOF'
basicConstraints=CA:FALSE
keyUsage=digitalSignature,keyEncipherment
extendedKeyUsage=clientAuth
EOF
    serial="$(openssl rand -hex 16)"
    openssl x509 -req -days 2 -sha256 \
        -in "$work/client.csr" -CA "$client_ca" -CAkey "$client_key" \
        -set_serial "0x$serial" -extfile "$work/client.ext" \
        -out "$work/client.crt" >/dev/null 2>&1

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
    chown -R 65532:65532 "$work"
}

render_metrics_resources "$OUTPOST_METRICS_ADDRESS" > "$work/resources.yaml"

# Upgrade the failed headless prototype in place. APIService's resolver
# refuses ClusterIP=None, and Service clusterIP is immutable, so a one-time
# exact-name delete is required before the corrected ordinary Service can be
# allocated. No workloads or data hang from this Service.
old_cluster_ip="$(k3s kubectl --kubeconfig="$internal_kubeconfig" \
    get service metrics-server -n kube-system \
    -o jsonpath='{.spec.clusterIP}' 2>/dev/null || true)"
if [ "$old_cluster_ip" = "None" ]; then
    log "migrating headless metrics Service to a private ClusterIP"
    k3s kubectl --kubeconfig="$internal_kubeconfig" \
        delete service metrics-server -n kube-system --wait=true >/dev/null 2>&1 || true
fi

# The apiserver may have opened its socket before admission/RBAC are ready.
# Retry for two minutes, then fail the whole control-plane container loudly.
i=0
until k3s kubectl --kubeconfig="$internal_kubeconfig" apply -f "$work/resources.yaml" >/dev/null 2>&1; do
    i=$((i + 1))
    [ "$i" -lt 120 ] || { log "FATAL: could not register metrics API"; exit 1; }
    sleep 1
done

# The aggregation layer insists on routing APIService traffic through an
# ordinary Service ClusterIP. This control plane intentionally has no local
# k3s agent/kube-proxy, so make ONLY that allocated /32 local and bridge its
# TLS port to the unprivileged sibling process. The address exists solely in
# this container namespace; no host/LAN port is published.
service_ip="$(k3s kubectl --kubeconfig="$internal_kubeconfig" \
    get service metrics-server -n kube-system \
    -o jsonpath='{.spec.clusterIP}')"
[ -n "$service_ip" ] && [ "$service_ip" != "None" ] || {
    log "FATAL: metrics Service has no ordinary ClusterIP"
    exit 1
}
ip address add "$service_ip/32" dev lo 2>/dev/null || true
log "bridging private Service ${service_ip}:443 to ${OUTPOST_METRICS_ADDRESS}:${OUTPOST_METRICS_PORT}"
socat "TCP-LISTEN:443,bind=${service_ip},reuseaddr,fork" \
    "TCP:${OUTPOST_METRICS_ADDRESS}:${OUTPOST_METRICS_PORT}" \
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

while :; do
    mint_credentials
    log "serving on ${OUTPOST_METRICS_ADDRESS}:${OUTPOST_METRICS_PORT}"
    # Virtual runtimes implement Pod lifecycle, not the kubelet resource
    # metrics endpoint. Scraping them can never succeed and otherwise wakes
    # the control plane every metric-resolution interval just to log "no
    # address matched types [ExternalIP]". The != selector includes unlabeled
    # legacy real agents while excluding the canonical virtual runtime label.
    setpriv --reuid=65532 --regid=65532 --clear-groups \
        metrics-server \
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
        --metric-resolution=15s &
    metrics_pid=$!

    # Rotate after 24h. Polling also notices an unexpected child exit within
    # ten seconds and fails the control-plane container instead of going green
    # with a dead Metrics API.
    ticks=0
    while [ "$ticks" -lt 8640 ] && kill -0 "$metrics_pid" 2>/dev/null; do
        sleep 10
        ticks=$((ticks + 1))
    done
    if ! kill -0 "$metrics_pid" 2>/dev/null; then
        wait "$metrics_pid" || rc=$?
        log "FATAL: metrics-server exited unexpectedly (${rc:-0})"
        exit 1
    fi
    log "rotating metrics-server client certificate"
    stop_metrics
done
