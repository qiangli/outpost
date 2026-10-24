---
id: 5179741f54da
kind: bug
title: fleet upgrade puller treats release v0.32.0 (older code, 2026-10-07) as newer than v0.32.0-dev (2026-10-08) and would apply it; satellite fleet targets can be stale
seq: 1905
status: todo
priority: p2
created: 2026-10-23T20:17:53.190743Z
---

Found 2026-10-23: the ivy satellite's fleet target is release_id v0.32.0, commit ca9a85b9 (2026-10-07). semver ranks v0.32.0 above v0.32.0-dev, so a host on the 2026-10-08 v0.32.0-dev build accepts the target and gets OLDER code (a second outpost on lern did exactly this). Expected: compare build time or commit ancestry as well as semver, or keep satellite fleet targets in lockstep with the current release. Separately, the ivy target should be refreshed to the current release.
