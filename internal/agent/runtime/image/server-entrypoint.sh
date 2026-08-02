#!/bin/sh
# server-entrypoint.sh — run a SELF-HOSTED DKS control plane.
#
# The counterpart of entrypoint.sh (which runs `k3s agent`). This one runs
# the two halves of a control plane IN ONE NETWORK NAMESPACE:
#
#   frps        accepts worker frpc logins; publishes each worker's kubelet
#               on this container's loopback at 127.0.0.1:<kubelet-port>
#   k3s server  the apiserver, which dials those loopback ports to serve
#               `kubectl logs` / `exec` / `port-forward` / metrics
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
# Configure k3s's packaged metrics-server addon so it scrapes nodes using
# ExternalIP (127.0.x.y, patched by nodeaddr) and derived kubelet ports, with
# hostNetwork enabled so it can reach loopback listeners across tunnels.
log "writing /var/lib/rancher/k3s/server/manifests/metrics-server-config.yaml"
mkdir -p /var/lib/rancher/k3s/server/manifests
cat > /var/lib/rancher/k3s/server/manifests/metrics-server-config.yaml <<EOF
apiVersion: helm.cattle.io/v1
kind: HelmChartConfig
metadata:
  name: metrics-server
  namespace: kube-system
spec:
  valuesContent: |-
    hostNetwork:
      enabled: true
    args:
      - --kubelet-preferred-address-types=ExternalIP,InternalIP,Hostname
      - --kubelet-use-node-status-port
      - --kubelet-insecure-tls
EOF

set -- server \
    --disable-agent \
    --disable=traefik,servicelb \
    --write-kubeconfig-mode=644 \
    --https-listen-port="${OUTPOST_API_PORT}" \
    --tls-san=127.0.0.1 \
    --tls-san=localhost
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
set -- "$@" --kube-apiserver-arg=kubelet-preferred-address-types=ExternalIP,InternalIP,Hostname

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
    log "publishing apiserver 127.0.0.1:${OUTPOST_API_PORT} as stcp k3s-apiserver"
    frpc -c /tmp/frpc-publish.toml &
else
    log "WARNING: apiserver never listened on ${OUTPOST_API_PORT}; not published"
fi

# Exit if EITHER half dies. A control plane with a live apiserver and a dead
# frps accepts no workers, and one with a live frps and a dead apiserver
# accepts workers and serves nothing — both look "running" to the supervisor,
# so neither may be survived silently.
while kill -0 "$FRPS_PID" 2>/dev/null && kill -0 "$K3S_PID" 2>/dev/null; do
    sleep 5
done
log "a control-plane process exited; stopping so the supervisor recreates us"
kill "$FRPS_PID" "$K3S_PID" 2>/dev/null || true
wait "$K3S_PID" 2>/dev/null || true
exit 1
