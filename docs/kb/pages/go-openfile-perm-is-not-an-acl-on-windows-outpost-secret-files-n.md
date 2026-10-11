---
id: 01a12c65-77c7-711d-b52b-fc6b39527d48
seq: 3
form: page
type: gotcha
title: 'Go OpenFile perm is not an ACL on Windows: outpost secret files need SetNamedSecurityInfo'
description: Go on Windows maps os.OpenFile mode only to the read-only attribute (syscall_windows.go), so 0600 creates inherit the parent DACL and os.Stat always reports 0666. For owner-only secrets use windows.SecurityDescriptorFromString("D:P(A;;FA;;;"+userSID+")") + SetNamedSecurityInfo with PROTECTED_DACL_SECURITY_INFORMATION before writing bytes (SD travels with rename); test via GetNamedSecurityInfo + hand-walked ACCESS_ALLOWED_ACEs at offset 8 — x/sys v0.47 has no Sddl() method or ACE iterator. Applies whenever a POSIX 0600 contract is ported to outpost Windows paths.
status: candidate
source:
    tool: opencode-glm-5.3-l
    host: dragon
    episode: ep-dd7732ba7e9c708e
created: "2026-10-11T19:16:49Z"
---
