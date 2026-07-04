---
name: hello-tessaro-deploy
description: Deploy pipeline for hello-tessaro, as a bashy dag task file — the handler outpost invokes on the deploy baton.
---

# hello-tessaro — deploy pipeline

The deploy **handler**: outpost invokes `bashy dag deploy.md deploy-<env>` on the
target (qa/prod) host when the `deploy:<env>` baton arrives (push over the tunnel,
or the pull fallback). This file is committed and **code-reviewed** — it runs on
the target host with *that host's* credentials (`bashy secrets`); the dev
conductor never has them. See `docs/sdlc-deploy-targets-design.md`.

One binary, three deploy modes — pick with `DEPLOY_MODE` (default `native`):

```bash
bashy dag deploy.md deploy-qa                 # native bare-metal (wrapper .previous-swap)
DEPLOY_MODE=podman bashy dag deploy.md deploy-qa
DEPLOY_MODE=dks    bashy dag deploy.md deploy-prod
```

Release coordinates arrive as environment: `REF` (commit/tag), `ARTIFACT_URL`,
`SHA256`. The bare-metal path delegates to `deploy/wrapper.sh` (atomic swap +
`/healthz`/`/version` verify + rollback); the DKS path is git→Argo (commit the
image tag; Argo CD reconciles); podman pulls + restarts the image.

## Tasks

### deploy-preview
Build + run locally for review — the local-dev preview that unifies with the live
loop (same repo, same deploy.md; only the target differs). Expose it to peers over
the mesh: `outpost mesh service add hello-tessaro-preview 127.0.0.1:8080`.
Effects: write

```bash
DEPLOY_MODE=native "$BASHY" dag deploy.md deploy-dev
echo ">> preview: http://127.0.0.1:8080  (mesh-expose: outpost mesh service add hello-tessaro-preview 127.0.0.1:8080)"
```

### deploy-dev
Deploy to dev. dev is permissive and auto-deployed; this is the fast inner loop.
Effects: write

```bash
"$BASHY" dag deploy.md _deploy DEPLOY_ENV=dev
```

### deploy-qa
Deploy to qa (the pre-prod gate). Auto after dev is green.
Effects: write

```bash
"$BASHY" dag deploy.md _deploy DEPLOY_ENV=qa
```

### deploy-prod
Deploy to prod. Reached only after the `deploy:prod` baton — which `sdlc promote`
applies only when the run is approved (policy `prod_approval: required`).
Effects: write

```bash
"$BASHY" dag deploy.md _deploy DEPLOY_ENV=prod
```

### _deploy
Shared deploy body — resolves the mode and dispatches. Not called directly; the
`deploy-<env>` targets invoke it with `DEPLOY_ENV` set.
Effects: write

```bash
: "${DEPLOY_ENV:?set DEPLOY_ENV=dev|qa|prod}"
mode="${DEPLOY_MODE:-native}"
ref="${REF:-latest}"
echo ">> deploy hello-tessaro env=$DEPLOY_ENV mode=$mode ref=$ref"

# Prod/qa credentials live HERE, on the target host — never with the dev conductor.
eval "$(bashy secrets env 2>/dev/null || true)"

case "$mode" in
  native)
    # Atomic .previous-swap + /healthz+/version verify + rollback (all in wrapper.sh).
    ./deploy/wrapper.sh --env "$DEPLOY_ENV" \
      ${ARTIFACT_URL:+--url "$ARTIFACT_URL"} ${SHA256:+--sha256 "$SHA256"}
    ;;
  podman)
    img="ghcr.io/qiangli/hello-tessaro:${ref}"
    "$BASHY" podman pull "$img"
    "$BASHY" podman rm -f "hello-tessaro-$DEPLOY_ENV" 2>/dev/null || true
    "$BASHY" podman run -d --name "hello-tessaro-$DEPLOY_ENV" \
      -e APP_ENV="$DEPLOY_ENV" -e HELLO_TESSARO_ADDR="0.0.0.0:8080" \
      -e HELLO_TESSARO_SSO_SECRET="${HELLO_TESSARO_SSO_SECRET:-}" \
      -p "127.0.0.1:0:8080" "$img"
    ;;
  dks)
    # git→Argo: bump the image tag in the env's values file; Argo CD reconciles.
    tag="${ref}"
    yq -i ".image.tag = \"$tag\"" "chart/values-$DEPLOY_ENV.yaml"
    "$BASHY" git commit -am "deploy: hello-tessaro $tag -> $DEPLOY_ENV"
    "$BASHY" git push
    echo ">> committed image tag $tag to chart/values-$DEPLOY_ENV.yaml; Argo CD will reconcile"
    ;;
  *)
    echo "unknown DEPLOY_MODE=$mode (native|podman|dks)" >&2; exit 2 ;;
esac
echo ">> deploy-$DEPLOY_ENV ($mode) done"
```
