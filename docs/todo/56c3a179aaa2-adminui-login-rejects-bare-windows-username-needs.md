---
id: 56c3a179aaa2
kind: bug
title: 'adminui login rejects bare Windows username (needs MACHINE\user): handleLogin uses EqualFold, not hostauth.SameUser'
seq: 2768
status: todo
priority: p2
labels:
    - windows
    - admin
    - auth
created: 2026-11-11T08:25:30.735981Z
---

Found 2026-11-11 during the S605 tray smoke on the Windows test host (outpost v1.0.0-rc.23-dev): the loopback admin UI login accepts only 'MACHINE\user'; a bare 'user' gets 'invalid credentials'.

Root cause (code read): internal/agent/adminui/handlers.go handleLogin compares strings.EqualFold(req.User, hostauth.CurrentUser()). On Windows CurrentUser is the SAM form 'MACHINE\user'. Commit 22f722e (2026-06-10, 'auth: accept bare usernames across platforms (hostauth.SameUser)') moved /auth (auth.go:116) and the SSH password callback (ssh.go) to hostauth.SameUser but missed this handler.

Fix: use hostauth.SameUser(req.User, current) in handleLogin (keep authenticating 'current'), plus a handler test with a 'HOST\alice' current user and a bare 'alice' submission. Not a tray issue; S605 stays no-core-change.
