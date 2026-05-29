#!/bin/bash
# outpost-runtime entrypoint. Reads credentials from env, establishes
# the matrix-tunnel STCP visitor to cloudbox's embedded apiserver (so
# k3s-agent can dial it as 127.0.0.1:6443), and then exec's k3s agent.
# Designed for a privileged container that joins cloudbox's cluster as
# a real worker Node.

set -eu

: "${OUTPOST_AGENT_NAME:?required: cluster Node name (= outpost agent name)}"
: "${OUTPOST_NODE_TOKEN:?required: k3s join token (K10...::server:...)}"
: "${OUTPOST_CLOUDBOX_HOST:?required: e.g. ai.dhnt.io}"
: "${OUTPOST_CLOUDBOX_PORT:=443}"
: "${OUTPOST_API_PORT:=6443}"
: "${OUTPOST_STCP_SECRET:?required: STCP secret for cluster.k3s-apiserver}"
: "${OUTPOST_MATRIX_TOKEN:=}"
: "${OUTPOST_POD_CIDR:=}"
: "${OUTPOST_OVERLAY_LOGIN:=}"
: "${OUTPOST_OVERLAY_AUTHKEY:=}"

log() { printf '[runtime] %s\n' "$*" >&2; }

# Best-effort: load br_netfilter so /proc/sys/net/bridge/bridge-nf-call-*
# exist (kube-proxy needs them for ClusterIP NAT). Modprobe inside a
# container needs /lib/modules from the host — often a no-op but harmless.
modprobe br_netfilter 2>/dev/null || true

# Pre-create /etc/machine-id so kubelet doesn't log noisy errors.
[ -f /etc/machine-id ] || cat /proc/sys/kernel/random/uuid | tr -d - > /etc/machine-id 2>/dev/null || true

# Optional CNI conflist (Phase 3 cross-outpost pod networking).
# Skipped for single-node setups where pods all schedule locally.
if [ -n "${OUTPOST_POD_CIDR}" ]; then
    log "writing /etc/cni/net.d/10-outpost.conflist pod_cidr=${OUTPOST_POD_CIDR}"
    cat > /etc/cni/net.d/10-outpost.conflist <<EOF
{
  "cniVersion": "0.4.0",
  "name": "outpost",
  "plugins": [{
    "type": "outpost-cni",
    "pod_cidr": "${OUTPOST_POD_CIDR}",
    "bridge_name": "cbox0"
  }]
}
EOF
fi

# Optional overlay (Phase 3 Tailscale via Headscale on cloudbox).
if [ -n "${OUTPOST_OVERLAY_LOGIN}" ] && [ -n "${OUTPOST_OVERLAY_AUTHKEY}" ]; then
    log "starting tailscaled — login_server=${OUTPOST_OVERLAY_LOGIN}"
    mkdir -p /var/lib/tailscale /var/run/tailscale
    tailscaled --state=/var/lib/tailscale/tailscaled.state \
        --socket=/var/run/tailscale/tailscaled.sock \
        --statedir=/var/lib/tailscale --tun=tailscale0 \
        >/tmp/tailscaled.log 2>&1 &
    sleep 2
    UP_ARGS="--login-server=${OUTPOST_OVERLAY_LOGIN} --authkey=${OUTPOST_OVERLAY_AUTHKEY} --reset --accept-routes"
    [ -n "${OUTPOST_POD_CIDR}" ] && UP_ARGS="${UP_ARGS} --advertise-routes=${OUTPOST_POD_CIDR}"
    tailscale up ${UP_ARGS} || log "WARN tailscale up failed; continuing"
fi

# Matrix-tunnel client (frpc) — connects to cloudbox via WSS and
# opens an STCP visitor that binds 127.0.0.1:${OUTPOST_API_PORT}
# locally, tunneling each accepted TCP conn to cloudbox's embedded
# apiserver. This is what makes k3s-agent's --server=https://127.0.0.1:6443
# actually reach the apiserver behind cloudbox's NAT.
log "writing /tmp/frpc.toml: server=${OUTPOST_CLOUDBOX_HOST}:${OUTPOST_CLOUDBOX_PORT} visitor=k3s-apiserver→127.0.0.1:${OUTPOST_API_PORT}"
cat > /tmp/frpc.toml <<EOF
serverAddr = "${OUTPOST_CLOUDBOX_HOST}"
serverPort = ${OUTPOST_CLOUDBOX_PORT}
user = "${OUTPOST_AGENT_NAME}"

[auth]
method = "token"
token = "${OUTPOST_MATRIX_TOKEN}"

[transport]
protocol = "wss"

[transport.tls]
enable = false

[[visitors]]
name = "k3s-apiserver-visitor"
type = "stcp"
serverUser = "cloudbox"
serverName = "k3s-apiserver"
secretKey = "${OUTPOST_STCP_SECRET}"
bindAddr = "127.0.0.1"
bindPort = ${OUTPOST_API_PORT}
EOF

log "starting frpc (matrix tunnel + STCP visitor)"
frpc -c /tmp/frpc.toml >/tmp/frpc.log 2>&1 &
FRPC_PID=$!

# Wait for the STCP visitor to bind locally. The visitor LISTENS even
# before the publisher accepts a stream, so a successful connect to
# 127.0.0.1:6443 means the tunnel is at least architecturally in place.
for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do
    if (echo >/dev/tcp/127.0.0.1/${OUTPOST_API_PORT}) 2>/dev/null; then
        log "STCP visitor reachable on 127.0.0.1:${OUTPOST_API_PORT} (attempt $i)"
        break
    fi
    sleep 1
done

log "exec k3s agent --server=https://127.0.0.1:${OUTPOST_API_PORT} --node-name=${OUTPOST_AGENT_NAME}"
exec /usr/local/bin/k3s agent \
    --server="https://127.0.0.1:${OUTPOST_API_PORT}" \
    --token="${OUTPOST_NODE_TOKEN}" \
    --node-name="${OUTPOST_AGENT_NAME}" \
    --snapshotter=fuse-overlayfs \
    --kubelet-arg=address=127.0.0.1 \
    --kubelet-arg=feature-gates=KubeletInUserNamespace=true \
    --kubelet-arg=cgroups-per-qos=false \
    --kubelet-arg=enforce-node-allocatable=
