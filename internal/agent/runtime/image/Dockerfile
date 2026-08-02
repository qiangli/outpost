# outpost-runtime image — the Linux kubelet runtime an outpost spins up
# when cluster.runtimes.agent is on. The outpost daemon itself runs on the
# host OS (macOS, Linux, Windows); only this image runs k3s agent +
# tailscaled + the CNI plugin, isolated in a podman container.
#
# One outpost = one identity = one Node. The container's k3s-agent
# joins as Node <outpost.AgentName>, using credentials the outpost
# daemon passes in via env vars. The image carries no outpost-specific
# state; identity comes from runtime env.
#
# Build (host-side, via the outpost binary):
#   outpost cluster build-runtime
#
# Behind the scenes that command materializes this directory tree
# (Dockerfile + entrypoint.sh + cni/ source) from `//go:embed` to a
# tempdir and runs `podman build -t outpost-runtime:dev .` against it.
# Layers are cached locally by podman so subsequent rebuilds reuse the
# base images + apt/curl steps — only the small bits that change
# (entrypoint, CNI source) re-execute. The host needs network the
# first time for the public-registry pulls; cached runs are offline-OK.
#
# Run (the outpost daemon does this; manual for testing):
#   podman run -d --name <agent-name>-runtime --privileged \
#       -e OUTPOST_AGENT_NAME=<agent-name> \
#       -e OUTPOST_NODE_TOKEN=K10...::node:... \
#       -e OUTPOST_API_PORT=6443 \
#       -e OUTPOST_KUBELET_PORT=17050 \
#       -e OUTPOST_OVERLAY_LOGIN=https://cb/overlay/headscale \
#       -e OUTPOST_OVERLAY_AUTHKEY=tskey-... \
#       -e OUTPOST_POD_CIDR=10.42.5.0/24 \
#       outpost-runtime:dev

# ---------- Stage 1: outpost-cni builder ----------
# Compiles the per-node CNI plugin from its embedded source. Stdlib
# only — no module proxy needed at image-build time. Pinned to the
# same Go minor we use on the host so the runtime binary is consistent.
FROM golang:1.25-alpine AS cni-builder
WORKDIR /src
# Copy go.mod first so the (currently no-op) `go mod download` layer
# stays cached when only .go source changes. Mirrors the canonical Go
# Dockerfile pattern; future-proofs against the day outpost-cni grows
# a third-party dep.
COPY cni/go.mod ./
RUN go mod download
COPY cni/ ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /outpost-cni .

# ---------- Stage 2: runtime ----------
FROM debian:trixie-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
        iproute2 iptables nftables conntrack ethtool ca-certificates \
        curl util-linux kmod procps jq bash fuse-overlayfs \
        openssl socat \
    && rm -rf /var/lib/apt/lists/*
# openssl + socat terminate TLS in front of the frp overlay-control visitor:
# tailscale's ts2021 control protocol requires a TLS transport even when the
# login-server is http://, and cloudbox's tunnelled endpoint is plain HTTP.
# socat presents a loopback self-signed cert (trusted via the CA store at
# runtime) and forwards decrypted bytes to the frp visitor. See entrypoint.sh.

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

# Official metrics-server binary, matching the version embedded by k3s
# 1.36.1. The peer control plane has no local agent, so this runs as a sibling
# process in the k3s/frps network namespace rather than as a scheduled Pod.
# Verify the per-architecture checksum published on the signed v0.8.1 release.
ARG METRICS_SERVER_VERSION=v0.8.1
RUN case "$TARGETARCH" in \
        amd64) METRICS_SHA=dd2455a5902d100aab7e5f7f9bcd8f715b6d39f97f161bbe87e3c24f61fa296c ;; \
        arm64) METRICS_SHA=0a3a052c44111dd0cc4ee86729d1eb621dd12b064f4dfb92e012d420ef0fd06f ;; \
        *) echo "unsupported metrics-server architecture: $TARGETARCH" >&2; exit 1 ;; \
    esac && \
    curl -fsSL -o /tmp/metrics-server \
        "https://github.com/kubernetes-sigs/metrics-server/releases/download/${METRICS_SERVER_VERSION}/metrics-server-linux-${TARGETARCH}" && \
    echo "${METRICS_SHA}  /tmp/metrics-server" | sha256sum -c - && \
    install /tmp/metrics-server /usr/local/bin/metrics-server && \
    rm -f /tmp/metrics-server

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
    install /tmp/frp_${FRP_VERSION}_linux_${FRP_ARCH}/frps /usr/local/bin/frps && \
    rm -rf /tmp/frp.tgz /tmp/frp_*
# frps ships alongside frpc and is what a SELF-HOSTED control plane runs.
# It has to live in this image — not in the outpost process — because the
# apiserver reaches a worker's kubelet through a port frps publishes on
# LOOPBACK, and loopback is per network-namespace. With frps on the host and
# the apiserver in a container the two 127.0.0.1s are different machines'
# (literally so on macOS, where podman runs a VM), and every `kubectl logs`
# against a tunnelled node fails with a 502 while everything else works.
# Co-locating them is exactly what cloudbox does, and is why it works there.

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
# on every pod ADD/DEL (multi-outpost overlay mode only; single-node
# falls back to the standard bridge plugin above). Built from the
# embedded source in stage 1 so we don't have to ship a pre-compiled
# binary in the repo.
COPY --from=cni-builder /outpost-cni /opt/cni/bin/outpost-cni
RUN chmod +x /opt/cni/bin/outpost-cni && mkdir -p /etc/cni/net.d

# Entrypoint reads OUTPOST_* env, writes the CNI conflist, starts
# tailscaled (if overlay creds are set), then exec's k3s agent.
COPY frpc-supervisor.sh /usr/local/bin/frpc-supervisor.sh
COPY metrics-server-supervisor.sh /usr/local/bin/metrics-server-supervisor.sh
COPY entrypoint.sh /entrypoint.sh
# The control-plane half. One image, two roles: the supervisor picks the
# entrypoint, so an operator never has to keep two images in step.
COPY server-entrypoint.sh /server-entrypoint.sh
RUN chmod +x /entrypoint.sh /server-entrypoint.sh \
        /usr/local/bin/frpc-supervisor.sh /usr/local/bin/metrics-server-supervisor.sh

ENTRYPOINT ["/entrypoint.sh"]
