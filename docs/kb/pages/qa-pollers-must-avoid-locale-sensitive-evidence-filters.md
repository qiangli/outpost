---
type: lesson
title: QA pollers must avoid locale-sensitive evidence filters
description: For outpost release QA pollers, do not pipe lane detection or REMOTE-QA-PASS checks through tr/grep under bashy when LC_* may be C.UTF-8 on macOS. Use shell case patterns for uname normalization and preserve smoke logs while checking the sentinel with awk; otherwise a scheduled gate can report no refs while hiding the real runtime state.
status: candidate
source:
    tool: codex-gpt-5.5-b
    host: dragon
    episode: weave-issue-2
created: "2026-08-29T18:45:21Z"
---
