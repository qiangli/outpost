# Member access contract (draft)

This is a source-derived draft, not certified behavior. The executable probe below was captured from the installed binary, which differs from the pinned reference; certification of the pinned commit remains outstanding. The reference is commit 791fd29dac401492acfe597875846073bc48d33e.

## Rules

The coordination service's public grant-copy wire endpoint is `GET /api/v1/hosts/self/grants`, with `Authorization: Bearer <host token>`. A successful response is JSON containing `host`, `owner`, `revision`, `members`, `groups`, and `apps`; `examples/grants.json` illustrates the shape. No live service may be used in tests. Use a synthetic loopback HTTP server, following the fake grants server pattern in bashy `tests/group-shaping-e2e`.

A logged-in caller is the owner, an active mutual member, or another account. With a grant copy, the owner is admitted. Other callers must be members and match at least one app rule: a nonempty circles list must contain the member's circle, and, if either groups or emails is nonempty, a group or email must match. Email comparisons ignore case. An empty rule admits any member. No rules means owner only. Multiple sources admit if any copy admits. Before a copy arrives the configured paired owner alone is admitted. App access with `require_login` uses this grant gate for requests through the relay; a non-login app can accept anonymous requests. Direct requests cannot forge the tunnel ingress identity.

The daemon fetches copies at startup and every 30 seconds; failed pulls retain the last good copy. A denied identified caller can trigger a shared forced refresh, rate limited to two seconds and waited on for at most three seconds. Revision is a change tag, not a monotonic counter. The relay reconnects with bounded backoff; its unavailability must not take down the daemon.

For an app opting into trusted identity, outpost strips client-supplied identity headers and stamps identity only for a trusted tunnel request with a forwarded prefix. Relationship is `owner`, `member`, or `account`; an account that is not a member has its address withheld and receives a per-app pseudonym. Groups are sorted and comma joined. With an app secret, the relationship signature is lowercase hex HMAC-SHA256 of UTF-8 `v2\n<kind>\n<circle>\n<groups>\n<visitor>\n<user>\n<prefix>\n<unix-seconds>`. The pseudonym is the first 32 hex characters of HMAC-SHA256 over `visitor\n<app>\n<lowercase-trimmed-user>`. The timestamp is shared with the identity signature. The upstream must verify signatures before trusting these headers.

## Assertions from real runs

On 2026-11-27 the installed outpost `a32ccba` (not the pinned commit) was run with a throwaway HOME, a loopback fake and no live service. Each line is an exact probe result; failure exits nonzero. The relay was unreachable throughout. The fake grant server was stopped before the last request. The probe accepts both `signed-in` (observed on this older binary) and `account` (source-derived at the pin); this difference requires gate verification and must not be treated as a certified identity rule.

### A-01

```console
$ python3 probe.py outpost examples/grants.json
owner: 200
member: 200
account: 403
anonymous: 403
public: 200
account identity: pseudonym and signature; address withheld
relay unavailable: daemon running, member allowed
```
exit: 0

The probe checks that a signature is present, not that it verifies, and does not independently exercise pairing state, circle and group boundaries, or a refresh after the server stops. Those remain unverified. The installed binary reported `signed-in` for the account relationship, whereas the inspected pin specifies `account`.
