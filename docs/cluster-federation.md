# Karmada federation (host side)

A federation host publishes a [Karmada](https://karmada.io) control plane from
the DKS it already hosts, so other DKS clusters can register as members.
This page covers the host: turning it on, what the daemon does, and what each
surface reports. Joining a member is a separate verb.

## Prerequisites

- A hosted control plane: `outpost cluster control-plane on` (see `cluster-peer`).
- Linux or macOS. `karmadactl` has no Windows release, so every federation verb
  refuses on Windows.
- The relay between members has no per-member quota by default. Do not assume a
  cap; any limit is an explicit operator setting on the relay.

## Turn it on

```
outpost cluster federation on [--cluster-name NAME] [--api-port 15443]
outpost cluster federation            # status; add --json for machines
outpost cluster federation token      # reveal what a member needs (credentials)
outpost cluster federation off [--force]
```

`on` records the intent and restarts. The control-plane container is recreated
(its data volume is kept) with a second published proxy, `karmada-apiserver`,
alongside `k3s-apiserver`, plus a small retrying publisher that port-forwards
the Karmada apiserver Service inside the plane. The publisher retries forever
with a 2s to 30s backoff, so a cold start only delays federation.

The daemon then, idempotently and retrying until each step holds:

1. installs the `karmada` appstore built-in on the hosted plane;
2. materialises the Karmada admin kubeconfig at
   `~/.kube/outpost-federation/karmada.yaml` (mode 0600, directory 0700) with
   its server rewritten to `https://127.0.0.1:15443`;
3. computes the CA discovery hash (sha256 of the CA's SubjectPublicKeyInfo) and
   stores it. The federation id is `fed-<first 12 hex>`;
4. once Karmada is Ready, registers this host's own plane as a pull-mode member.

`status` reports role, Karmada phase, federation id, kubeconfig path, whether
the host plane is registered and the last error. It never prints a credential.

## Ports

| Port  | Use                                   |
|-------|---------------------------------------|
| 6443  | visitor (cluster this host joins)     |
| 16443 | hosted apiserver                      |
| 16444 | virtual-kubelet bridge                |
| 7000  | tunnel server                         |
| 15443 | federation apiserver (loopback only)  |

15443 is never published on the host; it is reached through the tunnel.

## Credentials

`token` mints a bootstrap token and prints the tunnel endpoint, tunnel token,
STCP secret, bootstrap token and CA hash. They are credentials: the REST route
is interactive-login only and the MCP reveal is its own tool
(`outpost_federation_token`). The printed join hint names environment variables
(`OUTPOST_FEDERATION_JOIN_TOKEN`, `OUTPOST_FEDERATION_STCP_SECRET`,
`OUTPOST_FEDERATION_BOOTSTRAP_TOKEN`) and never a literal secret.

## Surfaces

| Surface | Entry |
|---------|-------|
| CLI     | `outpost cluster federation [on\|off\|status\|token]` |
| REST    | `GET/POST /api/cluster/federation`, `GET /api/cluster/federation/token` |
| MCP     | `outpost_federation`, `_on`, `_off`, `_token` |
| Config  | `cluster.federation.{enabled,host,cluster_name,api_port,kubeconfig,ca_hash}` |

## Troubleshooting

- `karmada: installing` for a long time: the operator image pull dominates;
  check the plane's `karmada-system` namespace.
- `last error: ... refused` on `127.0.0.1:15443`: the loopback federation
  visitor is not up yet; registration retries on its own.
- `federation id` empty: Karmada has not stored its admin kubeconfig yet.
