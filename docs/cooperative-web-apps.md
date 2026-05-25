# Cooperative web apps

Guidelines for HTTP apps you register with outpost (so they work cleanly
behind cloudbox at `https://<cloudbox>/h/<host>/app/<name>/...`).

## What outpost gives you

Every proxied request carries:

| Header                | Value (example)                              |
| --------------------- | -------------------------------------------- |
| `X-Forwarded-Host`    | `ai.dhnt.io`                                 |
| `X-Forwarded-Proto`   | `https`                                      |
| `X-Forwarded-Prefix`  | `/h/dragon/app/lern-admin`                   |
| `X-Forwarded-For`     | client IP chain                              |
| `X-Periscope-User`    | authenticated email (when login required)    |

Outpost also rewrites absolute-path `Location` headers (`/admin/login →
{prefix}/admin/login`) as a safety net. Everything else is on the app.

## What your app should do

1. **Read `X-Forwarded-Prefix` once** and either:
   - emit `<base href="{prefix}/">` in `<head>` and use **relative paths**
     for every link and asset (`static/foo.png`, not `/static/foo.png`); or
   - prepend `{prefix}` to every root-relative URL you generate.
2. **Use `X-Forwarded-Host` / `X-Forwarded-Proto`** (not `Host` / `r.TLS`)
   when constructing full URLs — OAuth callbacks, email links, sitemaps,
   webhook registrations, anything stored.
3. **Scope cookies**: `Set-Cookie: ...; Path={prefix}/` or omit `Path`
   entirely. Without this, login cookies land at the wrong scope and the
   user appears logged-out on every navigation.
4. **Validate `Origin` against `X-Forwarded-Host`**, not against `Host`.
   Incoming `Origin` is the public host; `Host` is whatever the proxy
   forwarded.
5. **No hardcoded URLs in JavaScript**: don't ship `fetch('/api/foo')`
   or `new WebSocket('ws://localhost:8080/…')`. Read the prefix from
   `<base>`, a server-injected `<meta name="base-href">`, or a
   `window.__BASE_URL` global the page-rendering layer sets.
6. **Emit relative or prefix-aware `Location` redirects.** Outpost
   prepends the prefix for absolute paths, but full URLs with a bare
   host (`Location: http://localhost:8080/...`) still escape.

## What outpost does *not* rewrite

- Response bodies (HTML / CSS / JS). Absolute paths in the body that
  don't match the prefix will navigate the browser out of your mount.
- `Set-Cookie` `Path` / `Domain`. Out of scope for the proxy; scope
  cookies correctly in the app.
- URLs hardcoded into compiled JS bundles.

## Pre-flight checklist

- [ ] `<base href>` emitted from `X-Forwarded-Prefix`
- [ ] All links and asset references are relative
- [ ] Full URLs built from `X-Forwarded-Host` + `X-Forwarded-Proto`
- [ ] Cookies scoped to `{prefix}/` (or `Path` omitted)
- [ ] No `/api/...` strings hardcoded in JS
- [ ] WebSocket URLs derived from `window.location`, not `localhost`

If all six pass, the app works through cloudbox unmodified per mount.
