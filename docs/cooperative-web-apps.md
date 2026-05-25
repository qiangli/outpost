# Cooperative web apps

Guidelines for HTTP apps you register with outpost (so they work cleanly
behind cloudbox at `https://<cloudbox>/h/<host>/app/<name>/...`).

## What outpost gives you

Every proxied request carries the standard forwarding headers:

| Header                | Value (example)                              |
| --------------------- | -------------------------------------------- |
| `X-Forwarded-Host`    | `ai.dhnt.io`                                 |
| `X-Forwarded-Proto`   | `https`                                      |
| `X-Forwarded-Prefix`  | `/h/dragon/app/lern-admin`                   |
| `X-Forwarded-For`     | client IP chain                              |

When the app's **Trust cloudbox identity** toggle is on (admin UI per-app
checkbox), outpost also stamps the SSO header set — same names off-the-
shelf trusted-header SSO apps already accept:

| Header                | Value (example)                              | Notes                          |
| --------------------- | -------------------------------------------- | ------------------------------ |
| `Remote-User`         | `alice@example.com`                          | Authelia/oauth2-proxy standard |
| `Remote-Email`        | `alice@example.com`                          | Same                           |
| `Remote-Name`         | `alice`                                      | Display name (email local-part fallback) |
| `Remote-Groups`       | `admin` or `user`                            | Mirrors `X-Periscope-Role`     |
| `X-Periscope-User`    | `alice@example.com`                          | Back-compat with earlier docs  |
| `X-Periscope-Role`    | `admin` or `user`                            | Back-compat                    |

Outpost also rewrites absolute-path `Location` headers (`/admin/login →
{prefix}/admin/login`) as a safety net. Everything else is on the app.

### Trust boundary guarantee

Outpost always strips `Remote-*` and `X-Periscope-*` headers from the
outgoing request before re-stamping them. The stamp only happens when:

1. **Trust cloudbox identity** is on for this app, AND
2. The request has `X-Forwarded-Prefix` set (i.e. arrived through the
   matrix tunnel, not via a direct LAN/loopback hit).

A LAN attacker who reaches the loopback main listener and forges
`Remote-User: admin@example.com` will have that header stripped before
the upstream sees it. Apps configured for trusted-header SSO must still
ensure they are *not* reachable bypassing the proxy (e.g. don't expose
the upstream port to the LAN with no auth of its own).

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

## SSO via trusted headers

Once you flip **Trust cloudbox identity** on for the app, the upstream
gets a cloudbox-vouched email on every request. Most off-the-shelf apps
support this with one config flag:

### Grafana

```ini
# grafana.ini
[auth.proxy]
enabled = true
header_name = Remote-User
header_property = email
auto_sign_up = true     # JIT-creates the Grafana user on first hit
whitelist =             # leave empty; outpost only stamps for paired callers
```

### Forgejo / Gitea

```ini
# app.ini
[service]
ENABLE_REVERSE_PROXY_AUTHENTICATION = true
ENABLE_REVERSE_PROXY_AUTO_REGISTRATION = true
ENABLE_REVERSE_PROXY_EMAIL = true

[security]
REVERSE_PROXY_AUTHENTICATION_HEADER = Remote-User
REVERSE_PROXY_AUTHENTICATION_EMAIL  = Remote-Email
```

### Sonarr / Radarr / Lidarr / Prowlarr (\*arr stack)

In the GUI: Settings → General → Authentication → `External`. Set the
proxy header to `Remote-User`.

### Custom app (Node / Express)

```js
app.use((req, res, next) => {
  const email = req.get('Remote-User');  // or X-Periscope-User
  if (email && req.get('X-Forwarded-Prefix')) {
    req.user = await findOrCreateUser(email);  // JIT
  }
  next();
});
```

The cooperative-web-apps contract is the same whether you're configuring
a vendor app or your own code: read `Remote-User` (or `X-Periscope-User`),
trust it because outpost guarantees it only ever appears on requests
that came through the matrix tunnel.

## Bottom-up user provisioning

For the app's tile to appear on a cloudbox user's dashboard, cloudbox
needs to know that user has access. The app's admin is the source of
truth for "who's allowed in" — when they create a user, the app pushes
the grant up to cloudbox through outpost's relay:

```
   App admin creates "alice@example.com" in the app
                       │
                       ▼  HTTP POST, loopback, Bearer <provisioning_token>
   outpost  /_periscope/apps/<name>/users
                       │
                       ▼  matrix tunnel, Authorization: Bearer <access_token>
   cloudbox  /api/hosts/<host>/apps/<name>/grants
                       │
                       ▼
   sharee row created  →  Alice's cloudbox dashboard shows the tile
                          (immediately if Alice already has a cloudbox
                          account; otherwise on her first login)
```

### Endpoints

Mounted on the admin UI listener (default `http://127.0.0.1:17777`) —
the same stable address apps already use to reach their tile locally.
Authentication is per-app bearer; outpost rotates and surfaces the
token in the admin UI under the app row.

```
POST   /_periscope/apps/<name>/users
       Authorization: Bearer <provisioning_token>
       Content-Type:  application/json
       Body:          {"email": "alice@example.com", "name": "Alice", "role": "user"}
       Returns:       cloudbox's response (200 / 201 / 409 depending on
                      whether the grant was created or already existed)

DELETE /_periscope/apps/<name>/users/<email>
       Authorization: Bearer <provisioning_token>
       Returns:       cloudbox's response (204 on success)

GET    /_periscope/apps/<name>/users
       Authorization: Bearer <provisioning_token>
       Returns:       [{"email":"alice@example.com","name":"Alice","role":"user"}, ...]
                      (30-second cache; cloudbox remains the source of truth)
```

### Semantics

- **Idempotent upserts.** POSTing the same email twice creates the row
  once. Apps can safely re-sync on startup.
- **Pending grants.** When the email doesn't yet have a cloudbox
  account, cloudbox stores the grant by email; the tile appears on the
  user's dashboard the moment they register/log in with that email.
- **Outpost does not persist grants.** Restart drops the in-memory
  cache; cloudbox is the source of truth. The 30 s GET cache is
  opportunistic.
- **Token rotation invalidates immediately.** The admin UI's Rotate
  button mints a new token and the previous value stops authenticating
  the next call. Reconfigure the app's webhook with the new bearer.

### Operator wiring

1. Admin UI → Apps → enable **Trust cloudbox identity** for the app.
2. Copy the displayed provisioning bearer (Reveal/Copy buttons).
3. Configure the app to call the loopback endpoint on user create/delete.
   Most off-the-shelf apps need a webhook plugin; for code you control,
   the snippet above (Node, but any HTTP client works) is the whole
   surface.
