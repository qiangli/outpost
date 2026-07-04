# GitOps: dev / qa / prod

Three environments, one chart, promoted by git.

## Install (once)

```bash
kubectl apply -f gitops/applicationset.yaml    # into the argocd namespace
```

Argo CD creates three Applications — `hello-tessaro-{dev,qa,prod}` — each into its
own namespace, each rendering `chart/values.yaml` + `chart/values-<env>.yaml`.
`CreateNamespace=true` provisions the namespaces; the chart's default-deny
`NetworkPolicy` keeps the envs isolated from each other.

## Per-env SSO secret (qa/prod)

dev uses an inline secret (dev-only). qa/prod reference a pre-provisioned Secret
(`ssoSecretName`) so the value never lives in git — seal it per env:

```bash
# generate a random secret, seal it for the target namespace, commit the sealed form
SECRET=$(head -c32 /dev/urandom | base64)
kubectl create secret generic hello-tessaro-qa-sso \
  --namespace hello-tessaro-qa --dry-run=client \
  --from-literal=sso-secret="$SECRET" -o yaml \
  | kubeseal --format yaml > gitops/sealed-qa-sso.yaml     # commit this
```

The same value goes into the app's per-env cloudbox tile (`sso_secret`), so a dev
secret can never sign a prod request. See `sealed-secret.example.yaml`.

## Promote dev → qa → prod

The image is immutable — the SAME tag flows across envs; only the values file
changes:

```bash
# promote the tag that's live in dev up to qa
yq -i '.image.tag = "0.1.0"' chart/values-qa.yaml
git commit -am "promote hello-tessaro 0.1.0 → qa"     # Argo CD syncs qa

# promote qa → prod (GATE this PR with required review == the deploy:prod baton)
yq -i '.image.tag = "0.1.0"' chart/values-prod.yaml
git commit -am "promote hello-tessaro 0.1.0 → prod"
```

## Rollback

Revert the values-<env>.yaml commit (Argo CD reconciles to the prior revision),
or `argocd app rollback hello-tessaro-<env> <revision>`.
