---
id: ffd9f0a521d6
kind: bug
title: 'Windows: when outpost starts from a boot task (S4U, session 0), ssh/exec sessions get an EMPTY PATH, so powercfg/schtasks/quser are ''command not found'''
seq: 1906
status: todo
priority: p1
labels:
    - windows
created: 2026-10-24T01:21:44.627036Z
sprint: 495
sprint_id: d7c905ee-3395-5fb3-82aa-b1585a02530e
sprint_title: 'Seamless onboarding: bashy onboard, setup-recipe rods, install/signing/winget'
---

Found 2026-10-23 on elephant after the headless reboot test: outpost started by the AtStartup S4U task 'outpost-boot' (no user logged in). In an ssh session to its sshd (:2222), echo $PATH prints an empty string and powercfg/schtasks/quser are not found; full paths (C:/Windows/System32/powercfg.exe) work. S4U tasks start with a minimal environment and the daemon passes it through. Expected: the daemon builds a sane session environment (the machine PATH from the registry plus the user PATH, SystemRoot, ComSpec, PATHEXT) for shell/exec/sftp sessions, the same as an interactive logon. Part of the outpost no-sleep/boot defaults (702c08bd): the post-install reboot must-check should assert that a basic command (e.g. 'where powercfg') resolves in a session after the reboot.
