# outpost-runtime image — the Linux kubelet runtime an outpost spins up
# when --cluster-mode=agent is on. The outpost daemon itself runs on the
# host OS (macOS, Linux, Windows); only this image runs k3s agent +
# tailscaled + the CNI plugin, isolated in a podman container.
#
# One outpost = one identity = one Node. The container's k3s-agent
# joins as Node <outpost.AgentName>, using credentials the outpost
# daemon passes in via env vars. The image carries no outpost-specific
# state; identity comes from runtime env.
#
# Build (cross-compile outpost-cni first; outpost daemon doesn't go in):
#   GOOS=linux GOARCH=arm64 go build -o script/linux-outpost/outpost-cni ./cmd/outpost-cni
#   ycode podman build -t outpost-runtime:dev script/linux-outpost
#
# Run (the outpost daemon does this; manual for testing):
#   ycode podman run -d --name <agent-name>-runtime --privileged \
#       -e OUTPOST_AGENT_NAME=<agent-name> \
#       -e OUTPOST_NODE_TOKEN=K10...::node:... \
#       -e OUTPOST_API_PORT=6443 \
#       -e OUTPOST_OVERLAY_LOGIN=https://cb/overlay/headscale \
#       -e OUTPOST_OVERLAY_AUTHKEY=tskey-... \
#       -e OUTPOST_OVERLAY_POD_CIDR=10.42.5.0/24 \
#       outpost-runtime:dev

FROM debian:trixie-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
        iproute2 iptables nftables conntrack ethtool ca-certificates \
        curl util-linux kmod procps jq bash fuse-overlayfs \
    && rm -rf /var/lib/apt/lists/*

# Tailscale install — official script puts tailscale + tailscaled on PATH.
RUN curl -fsSL https://tailscale.com/install.sh | sh

# k3s binary — direct download (the get.k3s.io install script wants
# systemd which we don't have in this container).
ARG K3S_VERSION=v1.36.1+k3s1
ARG TARGETARCH=arm64
RUN K3S_BIN=k3s$([ "$TARGETARCH" = "arm64" ] && echo -arm64 || true) && \
    curl -fsSL -o /usr/local/bin/k3s \
        "https://github.com/k3s-io/k3s/releases/download/${K3S_VERSION}/${K3S_BIN}" && \
    chmod +x /usr/local/bin/k3s && \
    /usr/local/bin/k3s --version | head -2

# frp client (matrix-tunnel client) — establishes the WSS tunnel to
# cloudbox + the STCP visitor that exposes cloudbox's embedded
# apiserver as 127.0.0.1:6443 inside the container. Pinned to the
# version cloudbox's serverdial publisher uses.
ARG FRP_VERSION=0.68.1
RUN FRP_ARCH=$([ "$TARGETARCH" = "arm64" ] && echo arm64 || echo amd64) && \
    curl -fsSL -o /tmp/frp.tgz \
        "https://github.com/fatedier/frp/releases/download/v${FRP_VERSION}/frp_${FRP_VERSION}_linux_${FRP_ARCH}.tar.gz" && \
    tar -xzf /tmp/frp.tgz -C /tmp && \
    install /tmp/frp_${FRP_VERSION}_linux_${FRP_ARCH}/frpc /usr/local/bin/frpc && \
    rm -rf /tmp/frp.tgz /tmp/frp_*

# Standard CNI reference plugins (bridge, host-local, loopback, portmap, …).
# Single-node mode uses bridge+host-local for pod IPs; multi-outpost mode
# overlays via outpost-cni on top. Without these the kubelet stays NotReady
# with "cni plugin not initialized" until something writes /etc/cni/net.d.
ARG CNI_PLUGINS_VERSION=v1.5.1
RUN CNI_ARCH=$([ "$TARGETARCH" = "arm64" ] && echo arm64 || echo amd64) && \
    mkdir -p /opt/cni/bin && \
    curl -fsSL \
        "https://github.com/containernetworking/plugins/releases/download/${CNI_PLUGINS_VERSION}/cni-plugins-linux-${CNI_ARCH}-${CNI_PLUGINS_VERSION}.tgz" \
        | tar -xz -C /opt/cni/bin

# Cloudbox CNI plugin — kubelet inside the container shells out to this
# on every pod ADD/DEL (multi-outpost overlay mode only; single-node falls
# back to the standard bridge plugin above).
COPY outpost-cni /opt/cni/bin/outpost-cni
RUN chmod +x /opt/cni/bin/outpost-cni && mkdir -p /etc/cni/net.d

# Entrypoint reads OUTPOST_* env, writes the CNI conflist, starts
# tailscaled (if overlay creds are set), then exec's k3s agent.
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

ENTRYPOINT ["/entrypoint.sh"]
