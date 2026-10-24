---
id: 936549d486b9
kind: bug
title: outpost register under an XDG_CONFIG_HOME override still rewrites the host's global login agent (launchd io.dhnt.outpost)
seq: 1903
status: todo
priority: p2
created: 2026-10-23T20:17:48.950023Z
---

Found 2026-10-23: 'XDG_CONFIG_HOME=<scratch> XDG_CACHE_HOME=<scratch> outpost register --server <sat> --code ... --name <client>' on macOS printed 'replaced a prior outpost registration that this command manages / registered launchd agent io.dhnt.outpost (--user)' and rewrote ~/Library/LaunchAgents/io.dhnt.outpost.plist, which belongs to the real install. No harm this time because the plist carries no XDG env, but a scratch or second pairing must never touch the host's service. Expected: register installs or touches the service only for the default config dir, or with an explicit --service.
