# Onboarding admin & regular users

Generalized from the classgo three-provision runbook. Adding a **web user** to a
tessaro app touches up to three places; adding a **web admin** touches all three.
Do them in order — a half-onboarded user hits one of the failure modes below.

Roles recap: **superadmin** = the process owner on the LAN (OS auth, never cloud);
**admin** = cloud-vouched + on the app's admin allowlist; **user** = cloud-vouched;
**public** = anyone (the `/` tile).

## Add a regular user (can reach `/app`)

1. **Cloudbox share row** — as the app owner in the cloudbox UI, *Invite member* on
   the host, share the app tile (`hello-tessaro-<env>`). The row
   `(Owner, Host, App, MemberEmail)` is the credential; the invitee gets a magic
   link and sees the tile under "Shared with me."

That's it for a regular user — `require_login:true` + a share row is enough to
reach `/app`. No OS account, no allowlist entry.

## Add an admin (can reach `/admin`)

1. **Cloudbox share row** — as above (admins are shared like anyone else; cloudbox
   stamps `Remote-Groups: admin` for every vouched caller — that is cloud tier, not
   app admin).
2. **App admin allowlist** — add the person's email to the app's admin list, either:
   - env: `HELLO_TESSARO_ADMIN_EMAILS="boss@x.io,staff@x.io"`, or
   - `config.<env>.json` → `"admin_emails": ["boss@x.io"]`.
   Restart the app (no hot-reload). This is what `RequireAdmin` checks — the cloud
   stamp alone is not enough (a non-listed cloud-admin caller gets 403 on `/admin`).

## Superadmin (LAN + OS auth)

Superadmin is **not** grantable from the cloud. `/admin/danger` is `lan_only_paths`
(outpost 404s it for web callers) and gated by `RequireLocal`. A real app adds an
OS-auth step there (a PAM `/admin/login` against the operator's own OS account).
The scaffold leaves that as a documented extension point (`TODO(superadmin)` in
the `coopauth` SDK (`coreutils/pkg/coopauth/guard.go`)).

## Remove a user (reverse order)

1. Delete the cloudbox share row.
2. Remove the email from `admin_emails` (if an admin) and restart.

## Half-onboarded failure modes

| Symptom | Missing step |
|---|---|
| 403 at cloudbox before the app loads | no share row |
| Reaches `/app` but 403 on `/admin` | not in `admin_emails` |
| `/admin/danger` returns 404 over the web | working as intended (LAN-only) |
| Cloud request 403 with a valid share | `require_hmac` on but `HELLO_TESSARO_SSO_SECRET` unset/mismatched |

## Per-environment note

Each env (dev/qa/prod) is a separate tile with its **own share list and its own
`sso_secret`**. Onboarding to qa does not grant prod — provision each env
explicitly. This is the isolation boundary the env model relies on.
