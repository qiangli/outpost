# Member access contract

This contract describes only the public wire protocol to the coordination service and the observable behaviour at `/app/<name>`. Reference runs use a synthetic loopback fake grants server (the fake grants server pattern from bashy `tests/group-shaping-e2e`: a loopback HTTP server serving a canned grant copy, a throwaway HOME/TMPDIR, and `OUTPOST_INGRESS_TRUST_LOOPBACK=1`), never a live service. The reference is commit 791fd29dac401492acfe597875846073bc48d33e. Evidence below was captured from the installed binary `a32ccba`, which differs from the pin in one labelled place; certification of the pin on the gate host remains outstanding.

## Rules

The coordination service's public grant-copy wire endpoint is `GET /api/v1/hosts/self/grants`, with `Authorization: Bearer <host token>`. A successful response is JSON containing `host`, `owner`, `revision`, `members`, `groups`, and `apps`; `examples/grants.json` is the referenced fixture the probe serves and judges against: its `demo` rule requires circle `company` and group `support`, with members `member@example.test` (company/support), `outsider@example.test` (friends, outside the circle), and `nogroup@example.test` (company, outside the group). No live service may be used in tests.

Pairing state is the daemon's configured paired owner plus its grant copies. Before any copy arrives the configured paired owner alone is admitted. The daemon fetches copies at startup and every 30 seconds; failed pulls retain the last good copy. A denied identified caller can trigger a shared forced refresh, rate limited to two seconds and waited on for at most three seconds. Revision is a change tag, not a monotonic counter. The relay reconnects with bounded backoff; its unavailability must not take down the daemon, and the last good copy stays effective while the coordination endpoint is unreachable.

A logged-in caller is the owner, an active mutual member, or another account. With a grant copy, the owner is admitted. Other callers must be members and match at least one app rule: a nonempty circles list must contain the member's circle, and, if either groups or emails is nonempty, a group or email must match. Email comparisons ignore case. An empty rule admits any member. No rules means owner only. Multiple sources admit if any copy admits. App access with `require_login` uses this grant gate for requests through the relay; a non-login app can accept anonymous requests. Direct requests cannot forge the tunnel ingress identity.

For an app opting into trusted identity, outpost strips client-supplied identity headers and stamps identity only for a trusted tunnel request with a forwarded prefix. Relationship is `owner`, `member`, or `account`; an account that is not a member has its address withheld and receives a per-app pseudonym. Groups are sorted and comma joined. With an app secret, the relationship signature `X-Outpost-Relationship-Sig` is lowercase hex HMAC-SHA256 of UTF-8 `v2\n<kind>\n<circle>\n<groups>\n<visitor>\n<user>\n<prefix>\n<unix-seconds>`, keyed by the app secret, with the timestamp in `X-Outpost-Identity-Ts`. The pseudonym `X-Outpost-Visitor` is the first 32 hex characters of HMAC-SHA256 over `visitor\n<app>\n<lowercase-trimmed-user>`. The upstream must verify signatures before trusting these headers.

## Assertions from real runs

On 2026-11-27 the installed outpost `a32ccba` (not the pinned commit) was run with a throwaway HOME, a loopback fake serving `examples/grants.json`, and no live service. Each line is an exact probe result; failure exits nonzero. The relay was unreachable throughout, and the fake grant server was stopped before the last request. The probe independently recomputes both HMACs above and requires them to match. It accepts both `signed-in` (observed on this older binary) and `account` (source-derived at the pin) as the account relationship label, verifying the signature over whichever label was stamped; this difference requires gate verification and must not be treated as a certified identity rule.

### A-01

```console
$ python3 probe.py outpost examples/grants.json
owner: 200
member: 200
member-outside-circle: 403
member-outside-group: 403
account: 403
anonymous: 403
public: 200
member identity: signed and verified (member/company/support)
account identity: pseudonym and signature verified; address withheld
relay unavailable: daemon running, member allowed
```
exit: 0

Pairing state beyond the pre-copy owner rule, and a forced refresh after a denial, are not independently exercised by this probe; they remain unverified until the gate-host run against the pinned build.
