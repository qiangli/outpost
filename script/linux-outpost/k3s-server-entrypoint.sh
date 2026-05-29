#!/bin/sh
set -eu

# Best-effort: load br_netfilter so /proc/sys/net/bridge/bridge-nf-call-*
# exist (kube-proxy needs them for ClusterIP NAT). Modprobe inside a
# container needs /lib/modules from the host — not present in our slim
# image, so this is often a no-op but harmless.
modprobe br_netfilter 2>/dev/null || true

# Pre-create /etc/machine-id so kubelet doesn't log a noisy error
# every startup. The value isn't security-sensitive for our use.
[ -f /etc/machine-id ] || cat /proc/sys/kernel/random/uuid | tr -d - > /etc/machine-id 2>/dev/null || true

exec /usr/local/bin/k3s server \
    --disable=traefik \
    --disable=servicelb \
    --snapshotter=fuse-overlayfs \
    --kubelet-arg=feature-gates=KubeletInUserNamespace=true \
    --kubelet-arg=cgroups-per-qos=false \
    --kubelet-arg=enforce-node-allocatable= \
    --write-kubeconfig-mode=644 \
    "$@"
