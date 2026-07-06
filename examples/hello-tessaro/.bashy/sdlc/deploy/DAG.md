---
vars:
  version: v0.0.1
---

# Deployment — dev / qa / prod

The **single deploy entry point** for this repo. `bashy dag .bashy/sdlc/deploy
deploy-<env>` runs the matching `deploy-<env>` target below.

**Version-tag idempotency.** Each env deploys under a version tag — `v0.0.1-dev`,
`v0.0.1-qa`, `v0.0.1` (prod, bare). Every target wraps its deploy in
`bashy sdlc deploy-once --version ${version} --env <env> -- <cmd>`, which runs
`<cmd>` **only if that tag isn't already on the remote**, then tags on success.
So flipping the `deploy:<env>` label back and forth (or an accidental relabel)
**never re-deploys**, and prod only ever ships the already-cut `${version}` —
never un-qa'd churn. **Cut a release by bumping `version` above** (`v0.0.1` →
`v0.0.2`).

The `deploy:<env>` **baton** (from `sdlc promote --env <env> --from <prev>`)
fires `.gitea/workflows/deploy.yml`, whose job `runs-on: [self-hosted, env:<env>]`
— so the deploy executes **on that env's host, with that host's creds** (`bashy
secrets`, host-scoped). The loom/dev side only *routes*; it never holds qa/prod
creds. Copy a `deploy-<env>` block per environment you run (or `include:` per-env
files into this folder).

## deploy-dev

```bash
bashy sdlc deploy-once --version ${version} --env dev -- \
  bash -c 'bashy dag build.md build && rsync -a ./out/ /srv/app-dev/'
```

## deploy-qa

```bash
bashy sdlc deploy-once --version ${version} --env qa -- \
  bash -c 'bashy dag build.md build && rsync -a ./out/ /srv/app-qa/'
```

## deploy-prod

Owner-gated: only reached after `sdlc promote --env prod --from qa` *and*
approval (`prod_approval`). `deploy-once` then no-ops unless `${version}` is a
new release.

```bash
bashy sdlc deploy-once --version ${version} --env prod -- \
  bash -c 'bashy dag build.md build && rsync -a ./out/ /srv/app/'
```
