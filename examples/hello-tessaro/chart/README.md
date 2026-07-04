# hello-tessaro Helm chart

Deploys `hello-tessaro` as a DKS pod — one binary, per-env config, isolated
per environment. Companion to the GitOps ApplicationSet in `../gitops/`.

```bash
helm lint . -f values.yaml -f values-dev.yaml
helm template ht . -f values.yaml -f values-prod.yaml   # inspect rendered manifests
```

## Values

| Key | Purpose |
|---|---|
| `env` | `APP_ENV` — dev/qa/prod |
| `image.{repository,tag}` | the image (same across envs; tag is the promotion unit) |
| `requireHmac` | reject cloud requests without a valid SSO-HMAC signature |
| `adminEmails` | app-internal admin allowlist (RBAC) |
| `ssoSecretName` | name of an existing (Sealed) Secret with key `sso-secret` — use in qa/prod |
| `ssoSecret` | inline secret; chart creates the Secret — **dev only** |
| `networkPolicy.enabled` | default-deny + DNS/same-namespace allow (env isolation) |
| `podDisruptionBudget.enabled` | HA guard (enable in prod) |

## Rendered resources

Deployment (non-root, read-only rootfs, `/healthz` probes) · Service (ClusterIP,
reached via `…/matrix/cluster/svc/<ns>/hello-tessaro/`) · NetworkPolicy
(default-deny) · Secret (dev inline only) · PodDisruptionBudget (prod).
