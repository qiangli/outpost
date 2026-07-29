---
type: lesson
title: Persistent file ledgers need inter-process serialization
description: Use when a CNI or other fork-per-operation tool allocates unique resources by scanning and writing a shared on-disk ledger.
tags:
    - cni
    - ipam
    - concurrency
    - flock
    - ledger
scope:
    repos:
        - outpost
    os: linux
status: validated
evidence: outpost aa65b9f; merged-tree CNI count=20, race count=3, Linux static build; independent review green
source:
    tool: qiangli
    host: dragon
created: "2026-07-29T16:46:32Z"
updated: "2026-07-29T16:46:37Z"
---

A persistent ledger prevents state loss across restarts but does not make read-choose-write allocation atomic. Separate CNI ADD processes can all scan the same free set and persist the same address under different container IDs. Hold a kernel-released inter-process lock across the complete existing-allocation check, used-set scan, selection, and durable commit; serialize DEL through the same lock. Persist via temp file, file fsync, rename, and directory fsync so readers never see partial claims. The regression must use barrier-synchronized child processes: goroutine-only coverage can be falsely satisfied by a process-local mutex. Existing ledgers also need fail-safe handling for duplicates, out-of-range addresses, malformed files, symlinks, and path-traversing IDs.
