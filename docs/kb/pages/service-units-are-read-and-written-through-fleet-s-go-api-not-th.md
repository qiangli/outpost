---
id: 01a12a0f-1b1e-7da4-bbc8-db64aae3b336
seq: 1
form: page
type: lesson
title: Service units are read and written through fleet's Go API, not the bashy CLI
description: 'When outpost needs a host''s service declarations or desired state, call fleet.Catalog (pkg/fleet: Services/Service/SaveService/ServicePortConflict) in process — it is the SAME store bashy service reads, with the same validation, so a toggle is one write instead of two paths that must agree. Only two things need the bashy binary: a PROCESS unit''s runtime state (bashy service status --json, the monitor''s facade over its status socket) and a dotted-path field override (bashy service set, because fleet''s path editor is unexported). An internal in-process capability needs no bashy at all. The superseded lesson about preserving bashy_services'' enabled-key presence no longer applies: bashy_services, bashy_apps_enabled, meet_enabled, loom_enabled and supervised were deleted in Sprint 418 S18 with no compatibility reader.'
status: candidate
source:
    tool: claude-opus5-c
    host: dragon
    episode: ep-dd7732ba7e9c708e
created: "2026-10-11T08:23:15Z"
supersedes: default-on-bashy-services-need-explicit-enabled-tracking
---
