#!/bin/sh
# server-entrypoint.sh — run a SELF-HOSTED DKS control plane.
#
# The counterpart of entrypoint.sh (which runs `k3s agent`). This one runs
# the two halves of a control plane IN ONE NETWORK NAMESPACE:
#
#   frps        accepts worker frpc logins; publishes each worker's kubelet
#               on this container's loopback at 127.0.0.1:<kubelet-port>
#   k3s server  the apiserver, which dials those loopback ports to serve
#               `kubectl logs` / `exec` / `port-forward`
#   metrics-server
#               an unprivileged sibling process that scrapes the same
#               authenticated loopback ports and serves `kubectl top`
#
# WHY THEY MUST SHARE A NAMESPACE, learned by shipping it wrong first: frp
# publishes a proxy's remotePort on 0.0.0.0, so any 127.0.0.0/8 address on
# that port reaches it — which is what lets the apiserver dial the unique
# per-node 127.0.x.y address the node-address reconciler assigns. But
# loopback is per-namespace. With frps in the outpost process on the host
# and the apiserver in a container, those are two different loopbacks (on
# macOS, two different machines — podman runs a VM). Scheduling and pod
# execution work; only logs/exec fail, with a 502 that reads like a network
# fault rather than a topology mistake.
#
# Required env:
#   OUTPOST_TUNNEL_TOKEN   shared secret gating frpc logins
#   OUTPOST_STCP_SECRET    secret authorizing visitors of the apiserver proxy
# Optional:
#   OUTPOST_TUNNEL_PORT    frps bind port (default 7000)
#   OUTPOST_API_PORT       apiserver port (default 6443)
#   OUTPOST_CLUSTER_CIDR / OUTPOST_SERVICE_CIDR
#   OUTPOST_TLS_SAN        extra comma-separated SANs
set -eu

log() { echo "[control-plane] $*" >&2; }

# listening probes via socat — netcat is not in this image, socat is (it is
# already used by the agent entrypoint's TLS shim).
listening() { socat -u /dev/null "TCP:127.0.0.1:$1,connect-timeout=1" 2>/dev/null; }

: "${OUTPOST_TUNNEL_TOKEN:?required: shared secret gating frpc logins}"
: "${OUTPOST_STCP_SECRET:?required: secret for the k3s-apiserver STCP proxy}"
: "${OUTPOST_TUNNEL_PORT:=7000}"
: "${OUTPOST_API_PORT:=6443}"
: "${OUTPOST_HOST_API_PORT:=16443}"

# k3s needs a non-loopback advertise address for the default kubernetes
# Service endpoint. The address is internal to this control-plane network
# namespace; peer workers continue to use their local STCP visitor URL.
ADVERTISE_ADDR="$(hostname -i | awk '{print $1}')"
case "$ADVERTISE_ADDR" in
    ""|127.*|::1)
        log "FATAL: no non-loopback container address for k3s advertise address"
        exit 1
        ;;
esac

# ---------------------------------------------------------------- frps ----
# bindAddr 0.0.0.0 so workers reach it through the container's published
# port. Its proxies' remotePorts likewise bind 0.0.0.0, which is what makes
# the per-node 127.0.x.y addresses dialable from the apiserver below.
cat > /tmp/frps.toml <<EOF
bindAddr = "0.0.0.0"
bindPort = ${OUTPOST_TUNNEL_PORT}

[auth]
method = "token"
token = "${OUTPOST_TUNNEL_TOKEN}"
EOF

log "starting frps on 0.0.0.0:${OUTPOST_TUNNEL_PORT}"
frps -c /tmp/frps.toml &
FRPS_PID=$!

# Give frps its listener before the publisher below dials it. A short bounded
# wait, not a sleep: a control plane that silently came up without its
# apiserver published would look healthy and serve nothing.
i=0
while [ "$i" -lt 50 ]; do
    if listening "${OUTPOST_TUNNEL_PORT}"; then break; fi
    i=$((i + 1))
    sleep 0.2
done
if ! listening "${OUTPOST_TUNNEL_PORT}"; then
    log "FATAL: frps did not listen on ${OUTPOST_TUNNEL_PORT}"
    exit 1
fi

# ------------------------------------------------- apiserver publisher ----
# A local frpc that publishes this container's own apiserver as the STCP
# proxy workers visit. Same shape as the worker side, pointed at ourselves.
cat > /tmp/frpc-publish.toml <<EOF
serverAddr = "127.0.0.1"
serverPort = ${OUTPOST_TUNNEL_PORT}
user = "control-plane"

[auth]
method = "token"
token = "${OUTPOST_TUNNEL_TOKEN}"

[[proxies]]
name = "k3s-apiserver"
type = "stcp"
secretKey = "${OUTPOST_STCP_SECRET}"
localIP = "127.0.0.1"
localPort = ${OUTPOST_API_PORT}
allowUsers = ["*"]
EOF

# --------------------------------------------------------- k3s server ----
set -- server \
    --disable-agent \
    --disable-cloud-controller \
    --disable=traefik,servicelb,metrics-server \
    --egress-selector-mode=pod \
    --write-kubeconfig=/etc/rancher/k3s/k3s-internal.yaml \
    --write-kubeconfig-mode=644 \
    --https-listen-port="${OUTPOST_API_PORT}" \
    --tls-san=127.0.0.1 \
    --tls-san=localhost \
    --advertise-address="$ADVERTISE_ADDR"
[ -n "${OUTPOST_TLS_SAN:-}" ] && for s in $(echo "${OUTPOST_TLS_SAN}" | tr ',' ' '); do
    set -- "$@" --tls-san="$s"
done

# Do NOT set --advertise-address=127.0.0.1. Kubernetes uses the advertise
# address for the default kubernetes Service endpoint and rejects loopback
# addresses there, causing controller-manager to exit during bootstrap.
# Workers keep using the K3S_URL configured on their own local STCP visitor;
# the server's internal, non-loopback advertise address is not their join URL.

# THE FLAG THE WHOLE TUNNELLED-KUBELET PATH DEPENDS ON. The apiserver dials
# a node using the first address type it finds in this list. Kubelet reports
# its own InternalIP, which for a tunnelled worker is a container-local
# address unreachable from here; the node-address reconciler publishes a
# reachable one as ExternalIP. Without ExternalIP FIRST the reconciler's
# work is ignored and every logs/exec against a remote node 502s.
# k3s WRAPS kube-apiserver rather than exposing its flags directly, so this
# must be passed through. Spelled as a k3s flag it is rejected and k3s
# prints its help and exits — which is how this was found.
# SINGLE value is intentional. A fallback list lets the apiserver accept the
# worker's self-reported InternalIP when the ExternalIP route is stale, then
# fail later with an opaque 502. ExternalIP is the reconciler-owned tunnel
# address; absence must fail closed instead of escaping the tunnel contract.
set -- "$@" --kube-apiserver-arg=kubelet-preferred-address-types=ExternalIP

# The peer node-address reconciler, not k3s's embedded cloud controller, owns
# each worker's ExternalIP. The embedded controller periodically rewrites the
# Node status from the worker's container address and removes that loopback
# ExternalIP; metrics then alternates between valid samples and "no address
# matched types [ExternalIP]". There is no cloud provider on a peer plane, so
# disabling that controller makes ownership explicit and keeps the tunnel
# address stable. Kubernetes controller-manager remains the sole PodCIDR
# allocator; this does not add or replace B5 allocation.

[ -n "${OUTPOST_CLUSTER_CIDR:-}" ] && set -- "$@" --cluster-cidr="${OUTPOST_CLUSTER_CIDR}"
[ -n "${OUTPOST_SERVICE_CIDR:-}" ] && set -- "$@" --service-cidr="${OUTPOST_SERVICE_CIDR}"

log "exec k3s $*"
k3s "$@" &
K3S_PID=$!

# Publish the apiserver once it answers. Doing this AFTER k3s starts avoids a
# proxy that points at a dead port during startup.
i=0
while [ "$i" -lt 300 ]; do
    if listening "${OUTPOST_API_PORT}"; then break; fi
    i=$((i + 1))
    sleep 1
done
if listening "${OUTPOST_API_PORT}"; then
    # k3s must run on its native internal port, but the host deliberately
    # publishes it on 16443 to avoid colliding with a worker's local visitor.
    # Export a host-usable kubeconfig without changing any internal component
    # kubeconfig.
    sed "s#server: https://127.0.0.1:${OUTPOST_API_PORT}#server: https://127.0.0.1:${OUTPOST_HOST_API_PORT}#" \
        /etc/rancher/k3s/k3s-internal.yaml > /etc/rancher/k3s/k3s.yaml.new
    chmod 0644 /etc/rancher/k3s/k3s.yaml.new
    mv -f /etc/rancher/k3s/k3s.yaml.new /etc/rancher/k3s/k3s.yaml

    # A scheduled metrics-server Pod cannot reach this namespace's loopback
    # FRP listeners, even with hostNetwork: it gets the WORKER host namespace.
    # Run the official binary here instead. The helper creates least-privilege
    # client credentials and a selectorless Service endpoint at ADVERTISE_ADDR;
    # no kubelet listener is published outside the authenticated tunnel.
    log "starting colocated metrics-server on ${ADVERTISE_ADDR}:10250"
    OUTPOST_METRICS_ADDRESS="${ADVERTISE_ADDR}" \
        /usr/local/bin/metrics-server-supervisor.sh &
    METRICS_PID=$!

    log "publishing apiserver 127.0.0.1:${OUTPOST_API_PORT} as stcp k3s-apiserver"
    frpc -c /tmp/frpc-publish.toml &
else
    log "WARNING: apiserver never listened on ${OUTPOST_API_PORT}; not published"
fi

# Exit if EITHER half dies. A control plane with a live apiserver and a dead
# frps accepts no workers, and one with a live frps and a dead apiserver
# accepts workers and serves nothing — both look "running" to the supervisor,
# so neither may be survived silently.
while kill -0 "$FRPS_PID" 2>/dev/null && kill -0 "$K3S_PID" 2>/dev/null && \
      kill -0 "${METRICS_PID:-0}" 2>/dev/null; do
    sleep 5
done
log "a control-plane process exited; stopping so the supervisor recreates us"
kill "$FRPS_PID" "$K3S_PID" "${METRICS_PID:-0}" 2>/dev/null || true
wait "$K3S_PID" 2>/dev/null || true
exit 1
