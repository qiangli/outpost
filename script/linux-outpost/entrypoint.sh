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

# CNI conflist. Two shapes:
#   1) Multi-outpost overlay (OUTPOST_POD_CIDR set + tailscaled below)
#      → outpost-cni plugin advertises this node's /24 via Tailscale.
#   2) Single-node / no overlay → standard bridge + host-local IPAM so
#      kubelet leaves NotReady and pods get IPs out of CNI_LOCAL_POD_CIDR
#      (default 10.43.42.0/24, well outside the host-side k3s cluster-cidr
#      so loopback routing doesn't collide).
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
else
    CNI_LOCAL_POD_CIDR="${CNI_LOCAL_POD_CIDR:-10.43.42.0/24}"
    log "writing /etc/cni/net.d/10-bridge.conflist (single-node, pod_cidr=${CNI_LOCAL_POD_CIDR})"
    cat > /etc/cni/net.d/10-bridge.conflist <<EOF
{
  "cniVersion": "0.4.0",
  "name": "cbr0",
  "plugins": [
    {
      "type": "bridge",
      "bridge": "cbr0",
      "isDefaultGateway": true,
      "forceAddress": false,
      "ipMasq": true,
      "hairpinMode": true,
      "ipam": {
        "type": "host-local",
        "ranges": [[{ "subnet": "${CNI_LOCAL_POD_CIDR}" }]]
      }
    },
    {
      "type": "portmap",
      "capabilities": { "portMappings": true }
    }
  ]
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

# Workload pods reach kube-apiserver via the kubernetes Service ClusterIP
# (10.43.0.1:443). kube-proxy DNATs that to the apiserver's address from
# the kubernetes Endpoints object — which kube-apiserver auto-populates
# with its own container IP (e.g. DO App Platform's 100.127.x.y), not
# reachable from this agent. Pre-empt kube-proxy by inserting our own
# DNAT at PREROUTING/OUTPOST position 1 that targets the local STCP
# visitor on loopback. route_localnet=1 is required so forwarded packets
# (pod → bridge → here) can be DNAT'd to a loopback address.
APISERVER_SVC_IP="${APISERVER_SVC_IP:-10.43.0.1}"
APISERVER_SVC_PORT="${APISERVER_SVC_PORT:-443}"
log "installing apiserver service DNAT: ${APISERVER_SVC_IP}:${APISERVER_SVC_PORT} -> 127.0.0.1:${OUTPOST_API_PORT}"
sysctl -w net.ipv4.conf.all.route_localnet=1 >/dev/null
for iface in lo eth0 default cbr0; do
    sysctl -w net.ipv4.conf.${iface}.route_localnet=1 2>/dev/null >/dev/null || true
done
# Watchdog: kube-proxy reconciles iptables every ~30s and re-inserts its
# KUBE-SERVICES jump at PREROUTING position 1, displacing ours. Without
# this loop, the kube-proxy DNAT for the kubernetes Service (10.43.0.1)
# fires first and sends pod traffic to cloudbox's unreachable mesh IP.
# Re-arm position 1 every 3s so our DNAT consistently wins.
(
    while true; do
        if ! iptables -t nat -L PREROUTING --line-numbers -n 2>/dev/null | head -3 | grep -q "^1 .*${APISERVER_SVC_IP}.*to:127.0.0.1:${OUTPOST_API_PORT}"; then
            iptables -t nat -D PREROUTING -d "${APISERVER_SVC_IP}/32" -p tcp --dport "${APISERVER_SVC_PORT}" \
                -j DNAT --to-destination "127.0.0.1:${OUTPOST_API_PORT}" 2>/dev/null || true
            iptables -t nat -I PREROUTING 1 -d "${APISERVER_SVC_IP}/32" -p tcp --dport "${APISERVER_SVC_PORT}" \
                -j DNAT --to-destination "127.0.0.1:${OUTPOST_API_PORT}" 2>/dev/null || true
        fi
        sleep 3
    done
) &
# OUTPUT chain isn't touched by kube-proxy, so one shot is enough.
iptables -t nat -I OUTPUT 1 -d "${APISERVER_SVC_IP}/32" -p tcp --dport "${APISERVER_SVC_PORT}" \
    -j DNAT --to-destination "127.0.0.1:${OUTPOST_API_PORT}"
# Arm cbr0.route_localnet=1 once the CNI bridge appears (kubelet creates
# it lazily on first pod).
(
    while ! ip link show cbr0 >/dev/null 2>&1; do sleep 2; done
    sysctl -w net.ipv4.conf.cbr0.route_localnet=1 >/dev/null 2>&1
    log "armed cbr0.route_localnet=1 after bridge creation"
) &

# Snapshotter selection: native `overlayfs` is preferred when the
# container's rootfs storage supports stacked overlay mounts AND we
# have CAP_SYS_ADMIN. Some podman storage backends (e.g. fuse-
# overlayfs-on-the-podman-side) break native overlay inside the
# container because the inner overlay can't stack on top of an outer
# overlay. The native path also occasionally hits "read-only file
# system" on /proc mounts inside the pod sandbox when the underlying
# rootfs comes from a podman named volume.
#
# fuse-overlayfs is the portable fallback — it adds a FUSE layer that
# survives any host rootfs config. Slightly slower but reliable across
# the matrix of podman storage configurations we see in the wild.
#
# Operators can override by exporting SNAPSHOTTER (e.g. SNAPSHOTTER=
# overlayfs) when launching the container if they know their host
# supports native overlay.
SNAPSHOTTER="${SNAPSHOTTER:-fuse-overlayfs}"
SNAPSHOTTER_ARGS="--snapshotter=${SNAPSHOTTER}"
log "snapshotter=${SNAPSHOTTER}"

# /etc/rancher/node is the node-identity directory (--with-node-id
# writes node-id + node-password.k3s here). The outpost daemon mounts
# this from a named volume keyed off the agent name so the identity
# persists across container restarts. Make sure the directory exists
# inside the container's filesystem; the volume mount overlays it.
mkdir -p /etc/rancher/node

log "exec k3s agent --server=https://127.0.0.1:${OUTPOST_API_PORT} --node-name=${OUTPOST_AGENT_NAME} --with-node-id ${SNAPSHOTTER_ARGS}"
exec /usr/local/bin/k3s agent \
    --server="https://127.0.0.1:${OUTPOST_API_PORT}" \
    --token="${OUTPOST_NODE_TOKEN}" \
    --node-name="${OUTPOST_AGENT_NAME}" \
    --with-node-id \
    ${SNAPSHOTTER_ARGS} \
    --kubelet-arg=address=127.0.0.1 \
    --kubelet-arg=feature-gates=KubeletInUserNamespace=true \
    --kubelet-arg=cgroups-per-qos=false \
    --kubelet-arg=enforce-node-allocatable=
