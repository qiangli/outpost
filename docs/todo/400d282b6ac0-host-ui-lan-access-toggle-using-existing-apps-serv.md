---
id: 400d282b6ac0
kind: feature
title: 'Host UI: LAN access toggle using existing Apps service settings'
seq: 1899
status: done
priority: p1
created: 2026-10-19T07:59:35.872678Z
assignee: codex-gpt6-sol
sprint: 470
sprint_id: 97636b80-98a2-5e03-ad69-f98ca8a7c507
sprint_title: Web console LAN access switch
closed: 2026-10-19T08:03:33.108117Z
closed_by: codex-gpt6-sol
---

Operator corrected scope: add one LAN on/off switch to existing Host Services Apps row. Reuse POST api/config/builtins service_settings for ports.0.bind and settings.pair. Do not change pairing, listeners, restart implementation or add APIs. Existing restart notice applies; no automatic restart. Focused check only.
