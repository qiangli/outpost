---
id: 84438d40447e
kind: bug
title: 'upgrade --direct: ''resolve latest release: unexpected EOF'' on rc.27-dev and rc.33-dev daemons; fleet pull did not move paired Windows hosts to rc.35'
seq: 2776
status: todo
priority: p2
labels:
    - release
    - upgrade
created: 2026-11-15T07:55:53.785614Z
---

Seen 2026-11-15 during the rc.35 rollout. (1) outpost upgrade --direct on the macOS QA host (rc.27-dev) and the first Windows host (rc.33-dev) failed twice with 'resolve latest release: unexpected EOF' while the dev box (a 563679a dev build) resolved the same GitHub latest release fine; --from <asset url> --sha256 worked on every host. Suspect the GitHub release API client in those builds (truncated body, HTTP/2 EOF or the no-token path); reproduce on a rc.33-dev binary, record the response, and make the resolver retry or report the HTTP status. (2) After cloudbox acknowledged the v1.0.0-rc.35 release webhook (07:29:54Z) the dev box puller saw target=v1.0.0-rc.35 within 10 min but declined by design (dev build not semver, never-downgrade), while both paired Windows hosts on rc.33-dev stayed on rc.33-dev for 20+ min with no upgrade log under their outpost profile dir; find where the Windows daemon logs puller decisions and why the pull/push did not apply. Hosts by role only; the fleet is on rc.35 via the explicit route.
