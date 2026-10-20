# Karmada federation

A federation host publishes a [Karmada](https://karmada.io) control plane from
the DKS it already hosts, so other DKS clusters can register as members.
This page covers the host (turning it on, what the daemon does, what each
surface reports) and then a member (`join` / `leave`).

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

On a host, 15443 is never published; it is reached through the tunnel. On a
member, the federation visitor binds 15443 inside the runtime container and is
published only on host loopback (`127.0.0.1:15443`).

## Credentials

`token` mints a bootstrap token and prints the tunnel endpoint, tunnel token,
STCP secret, bootstrap token and CA hash. They are credentials: the REST route
is interactive-login only and the MCP reveal is its own tool
(`outpost_federation_token`). The printed join hint names environment variables
(`OUTPOST_FEDERATION_JOIN_TOKEN`, `OUTPOST_FEDERATION_STCP_SECRET`,
`OUTPOST_FEDERATION_BOOTSTRAP_TOKEN`) and never a literal secret.

## Join a member

A member is another host with its own hosted control plane and an agent runtime
(the visitor lives in the runtime container). On the federation host run
`token`; on the member:

```
export OUTPOST_FEDERATION_JOIN_TOKEN=...      # tunnel token
export OUTPOST_FEDERATION_STCP_SECRET=...     # STCP secret
export OUTPOST_FEDERATION_BOOTSTRAP_TOKEN=... # bootstrap token
outpost cluster federation join <endpoint> --ca-hash sha256:<hex> [--cluster-name NAME]
outpost cluster federation             # follow progress
outpost cluster federation leave [--force]
```

`--token`, `--token-stdin`, `--stcp-secret` and `--bootstrap-token` are
accepted too; the environment keeps them out of argv and shell history.

`join` stores the endpoint, tunnel token, STCP secret, CA hash and cluster name
in the config. The bootstrap token (24 h, single use) is never written there: it
waits in a 0600 file under the user cache dir (`outpost/federation-join.json`)
and is deleted once registration succeeds. `join` then restarts. The runtime
container comes back with a third tunnel session to the host's frps carrying one
visitor, `karmada-apiserver`, bound on 15443. The member reconciler (every 30 s,
retrying sooner while not converged):

1. creates a selectorless Service `karmada-system/karmada-apiserver` (5443 to
   15443) and its Endpoints at this node's InternalIP. Endpoints can never be
   loopback; a loopback or stale address is replaced, and a recreated container
   on a new address is followed;
2. waits for `127.0.0.1:15443`, then runs
   `karmadactl register 127.0.0.1:15443 --token … --discovery-token-ca-cert-hash
   sha256:… --cluster-name NAME` against the plane's admin kubeconfig, and
   deletes the bootstrap token;
3. keeps two fixups applied: the agent's baked kubeconfig is repointed from
   `127.0.0.1:15443` to `karmada-apiserver.karmada-system.svc:5443`, and
   Deployment `karmada-agent` is pinned (nodeSelector) to this node.

Member `status` reports the endpoint, federation id, whether the join is still
pending, whether the visitor is bound, the mirror address, and whether
`karmada-agent` is installed and ready. `leave` (or `off` on a member) runs
`karmadactl unregister` while the visitor is still up, removes the mirror
Service/Endpoints and the pending token, clears the config and restarts without
the visitor. An unregister failure blocks unless `--force`. `karmadactl` is the
pinned, sha256-verified build from the managed CLI registry.

## Surfaces

| Surface | Entry |
|---------|-------|
| CLI     | `outpost cluster federation [on\|off\|status\|token\|join <endpoint>\|leave]` |
| REST    | `GET/POST /api/cluster/federation`, `GET /api/cluster/federation/token`, `POST /api/cluster/federation/{join,leave}` |
| MCP     | `outpost_federation`, `_on`, `_off`, `_token`, `_join`, `_leave` |
| Config  | `cluster.federation.{enabled,host,endpoint,token,stcp_secret,cluster_name,api_port,kubeconfig,ca_hash}` |

## Troubleshooting

- `karmada: installing` for a long time: the operator image pull dominates;
  check the plane's `karmada-system` namespace.
- `last error: ... refused` on `127.0.0.1:15443`: the loopback federation
  visitor is not up yet; registration retries on its own.
- `federation id` empty: Karmada has not stored its admin kubeconfig yet.
- Member `join pending: true` with `visitor bound: false`: the runtime container
  has not brought the visitor up (check the host's frps is reachable at the
  endpoint, and that this host has an agent runtime).
- Member `last error` mentioning an InternalIP: the node reports only loopback
  addresses; the mirror refuses to point Endpoints at loopback.
