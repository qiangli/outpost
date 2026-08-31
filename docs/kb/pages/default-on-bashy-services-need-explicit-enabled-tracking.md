---
type: lesson
title: Default-on bashy services need explicit enabled tracking
description: When a default-on BashyService also needs generated per-service SSO state, preserve whether the JSON enabled key was present. A generated minimal bashy_services row with only trust_cloud_identity/sso_secret must inherit the default service's enabled/app_port/auth metadata; otherwise SaveFile can serialize enabled:false and turn first boot into a durable opt-out.
status: candidate
source:
    tool: codex-gpt-5.5-a
    host: dragon
    episode: weave-issue-1
created: "2026-08-31T08:11:16Z"
---
