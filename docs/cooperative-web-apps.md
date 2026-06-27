# Cooperative web apps

Guidelines for HTTP apps you register with outpost (so they work cleanly
behind cloudbox at `https://<cloudbox>/matrix/h/<host>/app/<name>/...`).

## What outpost gives you

Every proxied request carries the standard forwarding headers:

| Header                | Value (example)                              |
| --------------------- | -------------------------------------------- |
| `X-Forwarded-Host`    | `ai.dhnt.io`                                 |
| `X-Forwarded-Proto`   | `https`                                      |
| `X-Forwarded-Prefix`  | `/matrix/h/host-a/app/lern-admin`            |
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

### What `Remote-Groups: admin` actually means

When an app reads `Remote-Groups`, the semantic is *"this caller cleared
cloudbox's OAuth + elevation gate at admin tier"* — not *"this caller is
an admin in your app."* The two layers can diverge:

- Cloudbox today stamps `admin` for every caller who clears elevation —
  owner and shared member alike. Under the binary share model, any
  sharee on a `require_login:true` app gets `admin` stamped at the
  proxy.
- For apps where every sharee being an app-admin is the right outcome
  (most home-lab scenarios — Plex for the family, a notes app for a
  partner), trust the header and move on.
- For apps where sharees should NOT be app-admins by default (a
  multi-tenant tool, a school system, anything with destructive ops),
  treat the cloud stamp as identity-only and apply your own RBAC on top
  — either against a roster the app already owns, or by requiring a
  second factor (local username/password) before granting elevated
  scope.

The classgo deployment on host-a is an example of the second pattern:
the cloud stamp gets the sharee through cloudbox + outpost, then
classgo's own OS-PAM `/admin/login` (against a separate host-a OS
account) gates app-level admin scope.

## Verifying the identity stamp (LAN-bypass defense)

The headers above only carry weight if the app can verify they actually
came from outpost. A process on the same LAN that reaches the upstream
port directly (bypassing outpost) could forge `Remote-User: admin@x`
trivially. Outpost defends against this by HMAC-signing the identity
tuple with a per-app shared secret.

### Per-app SSO secret

When the **Trust cloudbox identity** toggle is on, the admin UI displays
a per-app SSO secret. The upstream app and outpost share this secret
out-of-band — paste it into the app's config the same way you would a
JWT signing key. Rotate via the admin UI's "Rotate" button when needed;
the app reloads the new value on its next config refresh.

### Signed headers

For every proxied request that arrived through cloudbox AND has a
non-empty SSO secret configured, outpost stamps two extra headers:

| Header                       | Value (example)                    |
| ---------------------------- | ---------------------------------- |
| `X-Outpost-Identity-Ts`      | `1717392845`                       |
| `X-Outpost-Identity-Sig`     | `<hex sha256-hmac>`                |

The signature is `HMAC-SHA256(secret, payload)` over the canonical
payload (newline-separated, no trailing newline):

```
<Remote-User>
<Remote-Groups>
<X-Forwarded-Prefix>
<X-Outpost-Identity-Ts>
```

If `sso_secret` is empty, outpost stamps `Remote-*` headers without the
signature pair — the trust then rests entirely on outpost being the
only ingress (which is true when the upstream port is loopback-only).
Apps that read `Remote-User` SHOULD require a non-empty signature when
the upstream is reachable beyond loopback.

### Verification recipe

Reject the request when any of these checks fail:

1. `X-Forwarded-Prefix` is present (otherwise the caller isn't behind
   the proxy — apply your own LAN policy).
2. `X-Outpost-Identity-Ts` parses as a Unix timestamp and is within 60
   seconds of `now()` (the same window outpost enforces).
3. `X-Outpost-Identity-Sig` matches `HMAC-SHA256(secret, payload)`,
   compared with a constant-time equality function (Go
   `hmac.Equal` / Node `crypto.timingSafeEqual` / Python `hmac.compare_digest`).

#### Go

```go
func verifyOutpost(r *http.Request, secret []byte) bool {
    prefix := r.Header.Get("X-Forwarded-Prefix")
    user := r.Header.Get("Remote-User")
    role := r.Header.Get("Remote-Groups")
    ts := r.Header.Get("X-Outpost-Identity-Ts")
    sigHex := r.Header.Get("X-Outpost-Identity-Sig")
    if prefix == "" || ts == "" || sigHex == "" {
        return false
    }
    t, err := strconv.ParseInt(ts, 10, 64)
    if err != nil || abs(time.Now().Unix()-t) > 60 {
        return false
    }
    payload := user + "\n" + role + "\n" + prefix + "\n" + ts
    mac := hmac.New(sha256.New, secret)
    mac.Write([]byte(payload))
    want := mac.Sum(nil)
    got, err := hex.DecodeString(sigHex)
    if err != nil {
        return false
    }
    return hmac.Equal(got, want)
}
```

#### Node

```js
import { createHmac, timingSafeEqual } from "crypto"
function verifyOutpost(req, secret) {
  const prefix = req.get("X-Forwarded-Prefix") || ""
  const user   = req.get("Remote-User")        || ""
  const role   = req.get("Remote-Groups")      || ""
  const ts     = req.get("X-Outpost-Identity-Ts")  || ""
  const sigHex = req.get("X-Outpost-Identity-Sig") || ""
  if (!prefix || !ts || !sigHex) return false
  const t = parseInt(ts, 10)
  if (!Number.isFinite(t) || Math.abs(Math.floor(Date.now()/1000)-t) > 60) return false
  const payload = [user, role, prefix, ts].join("\n")
  const want = createHmac("sha256", secret).update(payload).digest()
  const got = Buffer.from(sigHex, "hex")
  return got.length === want.length && timingSafeEqual(got, want)
}
```

## Choosing an integration pattern

There are five recurring patterns. Pick whichever matches your app's
identity model; the admin-UI knobs (`require_login`, `lan_only_paths`,
`index_path`, `trust_cloud_identity`) compose to express all of them.

### A. Bring your own auth (multi-role)

Your app has its own user list and login UI; cloudbox is just transport.
Examples: Plex, Home Assistant, NextCloud, classgo.

- `require_login`: false for the public surface, true for the admin
  surface (use the multi-tile pattern below if both).
- `trust_cloud_identity`: optional — turn on if you want `Remote-User`
  for SSO; off if the app fully owns auth.
- `lan_only_paths`: anything destructive that should never be reachable
  from the web side (admin APIs, kiosk endpoints).
- The app's own login is the second layer of defense even when cloud
  identity is trusted.

### B. Trust the cloud completely (JIT-provisioned SSO)

Your app provisions users from the cloud-vouched email and never asks
for credentials. Examples: a custom dashboard, a personal wiki.

- `require_login`: true.
- `trust_cloud_identity`: true; SSO secret set; app verifies the HMAC.
- `lan_only_paths`: empty or just the routes you actively want to LAN-
  fence.
- Implement the verification recipe above before granting any session.
- Decide upfront: on unknown email, do you JIT-create a user, or 403?
  See "Unknown-email miss policy" below.

### C. No internal auth (cloudbox is the only gate)

Your app has zero auth — the proxy is the entire trust boundary.
Examples: a Jupyter notebook without a token, an IoT dashboard.

- `require_login`: true (cloudbox is your only gate, so don't skip it).
- `trust_cloud_identity`: usually off (no identity to read).
- `lan_only_paths`: any endpoint dangerous from the web (the IoT
  "restart device" route, the notebook kernel-shell, anything that
  presumes the same trust as physical access).
- The upstream port MUST stay loopback-only — if it's reachable from
  any LAN host, a sibling process bypasses the gate entirely.

### D. Public landing + private admin (multi-tile)

One upstream with two surfaces — a public face and an admin face.
Examples: a CMS blog, classgo's `lern` + `lern-admin` split.

- Register the SAME upstream twice with two different `name`s.
- Public tile: `require_login=false` + `lan_only_paths` listing every
  admin-or-API path you want hidden from the web.
- Admin tile: `require_login=true` + `index_path` pointing at the admin
  landing page (e.g. `/admin`).
- Both tiles share the SSO secret if `trust_cloud_identity` is on for
  either, so the upstream sees one HMAC key per app.
- Owner shares only the admin tile with admins; the public tile is
  open.

### E. API-only / headless

No browser surface — the app is consumed via Bearer tokens by scripts
and mobile clients.

- `require_login`: false (Bearer-only is its own auth).
- The Bearer is minted at cloudbox; the upstream does its own scope
  check against the token if needed.
- No `Remote-*` headers in this path; if the call needs cloud identity,
  pass it explicitly in the JSON payload.

## The three decisions every app author has to make

Before wiring an app into outpost, answer these:

1. **Where does the identity for a request come from?** Cloud-vouched
   (`Remote-User`), app-internal login, or both (cloud-vouched +
   second-factor)? Once you pick, document it in the app's CLAUDE-style
   notes so future contributors don't reintroduce a bypass.
2. **What's the unknown-email miss policy?** When `Remote-User` is a
   new email the app's roster doesn't know:
   - JIT-create (most home-lab apps, vendor SSO apps like Grafana).
   - Fall through to local login (apps with their own pre-existing
     user list — classgo does this).
   - Hard 403 (multi-tenant apps where every user is provisioned
     out-of-band).
   The default in most off-the-shelf SSO integrations is JIT — fine for
   single-owner deployments, possibly wrong for multi-tenant.
3. **Which paths should never be reachable from the web?** Walk the
   app's route table once and list every destructive or
   physical-presence-only path. Put them in `lan_only_paths`. The fence
   is checked before `require_login`, so even an authenticated admin
   from the web cannot reach them. The intent is "this path is
   physically dangerous from off-LAN; the fence catches operator
   mistakes and a future cloudbox bug both."

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
