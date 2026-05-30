# Joining a DKS cluster with a GPU outpost

cloudbox's DKS (the embedded k3s control plane your outpost joins as a
node) supports GPU workloads when:

1. The outpost host has an **NVIDIA driver** installed.
2. The host has the **NVIDIA container toolkit** configured for
   containerd.
3. The outpost runs in **real k3s-agent mode** (`outpost builtins set
   --cluster-mode=agent` — the default since `20d3d14`).
4. The cluster admin has installed the **`gpu-device-plugin` bundle**
   on cloudbox (`POST /api/cluster/install/gpu-device-plugin`, or via
   the SPA's Cluster page).

This document covers steps 1–3 (host setup). Step 4 is the cluster
admin's responsibility and happens once per cluster.

## 1. Verify driver

`nvidia-smi` from the host shell (NOT inside a container) must succeed
and list your GPU(s):

```
$ nvidia-smi
+-----------------------------------------------------------------------------+
| NVIDIA-SMI 535.86.10    Driver Version: 535.86.10    CUDA Version: 12.2     |
+-------------------------------+----------------------+----------------------+
| GPU  Name        Persistence-M| Bus-Id        Disp.A | Volatile Uncorr. ECC |
|...
```

If `nvidia-smi` fails or isn't installed:

- **Ubuntu/Debian**: `sudo apt install nvidia-driver-535` (or the
  current LTS driver). Reboot afterwards.
- **Arch / Fedora / RHEL**: install the distro's `nvidia` package per
  the distro's wiki.
- **Headless servers**: confirm Secure Boot doesn't block driver load
  (`mokutil --sb-state`); if enabled, enroll the NVIDIA module signing
  key or disable Secure Boot.

## 2. Install NVIDIA container toolkit

The toolkit teaches containerd to mount `/dev/nvidia*` devices into
containers when a Pod requests `nvidia.com/gpu`. Without this step,
the device plugin will see GPUs but containerd will start ML
containers without GPU access.

```
distribution=$(. /etc/os-release; echo $ID$VERSION_ID)
curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey | sudo gpg --dearmor -o /usr/share/keyrings/nvidia-container-toolkit-keyring.gpg
curl -s -L https://nvidia.github.io/libnvidia-container/$distribution/libnvidia-container.list | \
  sed 's#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg] https://#g' | \
  sudo tee /etc/apt/sources.list.d/nvidia-container-toolkit.list
sudo apt update && sudo apt install -y nvidia-container-toolkit

# Configure containerd to use nvidia runtime
sudo nvidia-ctk runtime configure --runtime=containerd --set-as-default
sudo systemctl restart containerd
```

(Source: https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/install-guide.html)

## 3. Switch outpost to real k3s-agent mode

The vkpodman cluster mode doesn't translate `resources.limits."nvidia.com/gpu"`
to host device mounts; use the real k3s-agent mode for ML workloads.

```
outpost builtins set --cluster-mode=agent
outpost restart   # restart picks up the new mode
```

Verify the outpost daemon is reporting GPUs to cloudbox:

```
outpost status --json | jq .system.gpus
[
  {
    "kind": "nvidia",
    "model": "NVIDIA GeForce RTX 4090",
    "count": 1,
    "vram_total_bytes": 25756172288
  }
]
```

## 4. (Cluster admin) install the device plugin bundle

```
curl -X POST -H "Cookie: session=<session-cookie>" \
  https://ai.dhnt.io/api/cluster/install/gpu-device-plugin
```

…or click the Install button on the SPA's Cluster → Bundles page.

## 5. Verify GPU is allocatable

Download a user kubeconfig (Cloudbox SPA → Cluster → Download kubeconfig),
then:

```
kubectl get nodes -o jsonpath='{.items[*].status.allocatable.nvidia\.com/gpu}'
1
```

If you see `0` or empty, check the device-plugin DaemonSet:

```
kubectl get pods -n kube-system -l name=nvidia-device-plugin-ds
kubectl logs -n kube-system <plugin-pod>
```

Common gotchas:

- `Failed to initialize NVML: could not load NVML library` — driver
  isn't installed on the host or doesn't match the toolkit version.
- DaemonSet pod is `Pending` — the host's containerd doesn't have the
  `nvidia` runtime configured; step 2 wasn't run or hasn't reloaded.

## 6. Smoke-test a Pod

```
kubectl run gpu-test --rm -it --restart=Never \
  --image=nvcr.io/nvidia/cuda:12.2.0-base-ubuntu22.04 \
  --overrides='{"spec":{"containers":[{"name":"gpu-test","image":"nvcr.io/nvidia/cuda:12.2.0-base-ubuntu22.04","resources":{"limits":{"nvidia.com/gpu":1}},"command":["nvidia-smi"]}]}}' \
  -- nvidia-smi
```

If you see the same `nvidia-smi` output you saw in step 1 — but from
inside a container, scheduled by k3s — the end-to-end chain works and
you're ready for Kubeflow / PyTorchJob / etc.

## Per-user workload quotas

Every cloudbox user's namespace (`user-<hash>`) gets these defaults
stamped on first kubeconfig fetch:

| Resource | Cap |
|---|---|
| CPU (req + limit) | 4 cores |
| Memory (req + limit) | 8 GiB |
| nvidia.com/gpu | 1 |
| Pods | 20 |
| PVCs | 5 |
| Storage (sum of PVC requests) | 100 GiB |

A Pod that omits `resources` gets defaults of 500m CPU / 512 MiB
memory (limit) and 100m / 128 MiB (request) per container, so it
counts against the quota even without explicit declaration.

To bump the caps for a power user, delete the `outpost-user-defaults`
ResourceQuota in their namespace; the next time they fetch a fresh
kubeconfig (via Cloudbox SPA → Cluster → Download), cloudbox re-stamps
the default — so set the bumped values on the cloudbox side (Tier-C+
work) rather than ad-hoc edits.
