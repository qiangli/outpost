#!/bin/sh
# outpost-runtime entrypoint. Reads credentials from env, writes the
# CNI conflist, starts tailscaled (if overlay configured), then exec's
# k3s agent. Designed to run as PID 1 inside a privileged podman
# container — the outpost daemon on the host orchestrates this from
# outside.

set -eu

: "${OUTPOST_AGENT_NAME:?required: the outpost's agent name (the cluster Node name)}"
: "${OUTPOST_NODE_TOKEN:?required: k3s join token (K10...::node:...)}"
: "${OUTPOST_API_PORT:=6443}"
: "${OUTPOST_API_SERVER:=https://127.0.0.1:${OUTPOST_API_PORT}}"
: "${OUTPOST_POD_CIDR:=}"
: "${OUTPOST_OVERLAY_LOGIN:=}"
: "${OUTPOST_OVERLAY_AUTHKEY:=}"

log() { printf '[runtime] %s\n' "$*" >&2; }

# Write CNI conflist for outpost-cni. Pod IPs come from POD_CIDR; if
# unset, leave the conflist absent and rely on k3s's defaults.
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

# Tailscaled (overlay) — only when login_server + authkey are present.
# Skip silently otherwise: single-node tests don't need the overlay.
if [ -n "${OUTPOST_OVERLAY_LOGIN}" ] && [ -n "${OUTPOST_OVERLAY_AUTHKEY}" ]; then
    log "starting tailscaled — login_server=${OUTPOST_OVERLAY_LOGIN}"
    mkdir -p /var/lib/tailscale
    tailscaled \
        --state=/var/lib/tailscale/tailscaled.state \
        --socket=/var/run/tailscale/tailscaled.sock \
        --statedir=/var/lib/tailscale \
        --tun=tailscale0 >/tmp/tailscaled.log 2>&1 &
    TAILSCALED_PID=$!
    sleep 2
    UP_ARGS="--login-server=${OUTPOST_OVERLAY_LOGIN} --authkey=${OUTPOST_OVERLAY_AUTHKEY} --reset --accept-routes"
    [ -n "${OUTPOST_POD_CIDR}" ] && UP_ARGS="${UP_ARGS} --advertise-routes=${OUTPOST_POD_CIDR}"
    log "tailscale up ${UP_ARGS}"
    if ! tailscale up ${UP_ARGS}; then
        log "WARN tailscale up failed; continuing without overlay"
    fi
else
    log "no overlay credentials — running without tailscale (single-node mode)"
fi

log "exec k3s agent --server=${OUTPOST_API_SERVER} --node-name=${OUTPOST_AGENT_NAME}"
exec /usr/local/bin/k3s agent \
    --server="${OUTPOST_API_SERVER}" \
    --token="${OUTPOST_NODE_TOKEN}" \
    --node-name="${OUTPOST_AGENT_NAME}" \
    --kubelet-arg=address=127.0.0.1
