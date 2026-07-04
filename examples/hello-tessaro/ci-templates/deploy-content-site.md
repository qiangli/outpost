---
name: content-site-deploy
description: COPY-ME deploy.md for a static / Next.js content site — GitHub Pages is a third-party deploy target, loom is the source of truth.
---

# content site — deploy pipeline (copy to repo-root `deploy.md`)

Reference `deploy.md` for a **content site** (static HTML / Next.js / Hugo / …)
whose **source of truth is loom**. GitHub Pages is just a **third-party static-host
target** you publish built output to — one target among many. The conductor edits
the loom repo; `bashy dag deploy.md deploy-<target>` runs on the mesh host.

Config (env / `bashy secrets`):
- `PAGES_REMOTE`  — the GitHub Pages repo to publish to, e.g.
  `git@github.com:owner/owner.github.io.git` (a publish destination, NOT the SoT).
- `PAGES_BRANCH`  — the branch GitHub Pages serves (default `gh-pages`).
- `BUILD_CMD`     — how to build (default `pnpm build`); `BUILD_OUT` — output dir (default `out`).

## Tasks

### build
Build the static site into $BUILD_OUT.
Sources: .
Generates: out
Effects: write

```bash
${BUILD_CMD:-pnpm build}
```

### deploy-preview
Serve the built site locally for review — the local-dev preview that unifies with
the live loop (same repo, same deploy.md). Expose to peers over the mesh:
`outpost mesh service add site-preview 127.0.0.1:8088`.
Requires: build
Effects: write

```bash
out="${BUILD_OUT:-out}"
( cd "$out" && python3 -m http.server 8088 >/tmp/site-preview.log 2>&1 & )
echo ">> preview: http://127.0.0.1:8088  (mesh-expose: outpost mesh service add site-preview 127.0.0.1:8088)"
```

### deploy-github-pages
THIRD-PARTY static-host target: publish the built output to the GitHub Pages branch.
GitHub just serves the branch — **no GitHub Action, no self-hosted runner.** loom
remains the source of truth; GitHub is a publish destination (like a droplet or an
object store). Prod creds (the deploy key / token for PAGES_REMOTE) live HERE, on
the target host — never with the dev conductor.
Requires: build
Effects: write

```bash
: "${PAGES_REMOTE:?set PAGES_REMOTE=git@github.com:owner/owner.github.io.git}"
branch="${PAGES_BRANCH:-gh-pages}"
out="${BUILD_OUT:-out}"
eval "$(bashy secrets env 2>/dev/null || true)"   # deploy key / token for PAGES_REMOTE

work="$(mktemp -d)"
# Reuse the existing Pages branch history when present; else start it fresh (orphan).
if git clone --depth 1 --branch "$branch" --single-branch "$PAGES_REMOTE" "$work" 2>/dev/null; then
  find "$work" -mindepth 1 -maxdepth 1 ! -name .git -exec rm -rf {} +
else
  git clone --depth 1 "$PAGES_REMOTE" "$work"
  git -C "$work" checkout --orphan "$branch"
  git -C "$work" rm -rq --cached . 2>/dev/null || true
  find "$work" -mindepth 1 -maxdepth 1 ! -name .git -exec rm -rf {} +
fi
cp -R "$out"/. "$work"/
touch "$work/.nojekyll"                 # serve _next/ etc. verbatim
git -C "$work" add -A
git -C "$work" -c user.email=sdlc@dhnt.io -c user.name=sdlc \
    commit -qm "publish $(git -C "$PWD" rev-parse --short HEAD 2>/dev/null || date -u +%FT%TZ)" || true
git -C "$work" push "$PAGES_REMOTE" "$branch"
rm -rf "$work"
echo ">> published $out -> $PAGES_REMOTE ($branch); GitHub Pages serves it"
```

### deploy-droplet
Alternative third-party target: rsync the built output to a server you control.
Requires: build
Effects: write

```bash
: "${DROPLET:?set DROPLET=user@host:/var/www/site}"
eval "$(bashy secrets env 2>/dev/null || true)"
rsync -az --delete "${BUILD_OUT:-out}/" "$DROPLET/"
echo ">> rsynced to $DROPLET"
```
