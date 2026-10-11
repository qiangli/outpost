---
id: 01a12a0f-1b20-7860-b37a-700c7a2b8d6c
seq: 2
form: page
type: lesson
title: Default-on bashy services need explicit enabled tracking
description: When a default-on BashyService also needs generated per-service SSO state, preserve whether the JSON enabled key was present. A generated minimal bashy_services row with only trust_cloud_identity/sso_secret must inherit the default service's enabled/app_port/auth metadata; otherwise SaveFile can serialize enabled:false and turn first boot into a durable opt-out.
status: superseded
source:
    tool: codex-gpt-5.5-a
    host: dragon
    episode: weave-issue-1
created: "2026-08-31T08:11:16Z"
updated: "2026-10-11T08:23:15Z"
superseded_by: service-units-are-read-and-written-through-fleet-s-go-api-not-th
---
