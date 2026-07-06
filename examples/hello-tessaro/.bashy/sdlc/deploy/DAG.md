# Deployment — dev / qa / prod

The **single deploy entry point** for this repo. `bashy dag .bashy/sdlc/deploy
deploy-<env>` runs the matching `deploy-<env>` target below.

The `deploy:<env>` **baton** (applied by `sdlc promote --env <env> --from <prev>`)
fires `.gitea/workflows/deploy.yml`, whose job `runs-on: [self-hosted, env:<env>]`
— so the deploy executes **on that env's host runner, with that host's creds**
(`bashy secrets`, host-scoped). The loom/dev side only *routes*; it never holds
qa/prod creds.

Copy a `deploy-<env>` block per environment you run. Keep them inline here for a
small repo; for a larger one, split per-env files into this folder and pull them
in with `include:` frontmatter (e.g. `include: [dev.md, qa.md, prod.md]`).

## deploy-dev

Runs on the **dev** host (runner label `env:dev`).

```bash
bashy dag build.md build                      # build once (creds/config from this host)
# ... your dev deploy: rsync ./out/ /srv/app/ · bashy podman up · bashy kubectl apply ...
echo "deployed dev @ $(git rev-parse --short HEAD)"
```

## deploy-qa

Runs on the **qa** host (runner label `env:qa`), with qa creds from that host's vault.

```bash
bashy dag build.md build
# ... same shape, qa target ...
echo "deployed qa @ $(git rev-parse --short HEAD)"
```

## deploy-prod

Runs on the **prod** host (runner label `env:prod`), with prod creds from that
host's vault. **Owner-gated:** only reached after
`sdlc promote --env prod --from qa` *and* approval (`prod_approval`), so this job
never fires unmet.

```bash
bashy dag build.md build
# ... same shape, prod target ...
echo "deployed prod @ $(git rev-parse --short HEAD)"
```
