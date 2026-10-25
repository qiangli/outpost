---
id: 936549d486b9
kind: bug
title: outpost register under an XDG_CONFIG_HOME override still rewrites the host's global login agent (launchd io.dhnt.outpost)
seq: 1903
status: done
priority: p2
created: 2026-10-23T20:17:48.950023Z
assignee: muse-spark1.2
sprint: 497
sprint_id: 16af0380-7ec7-5d41-a9c6-50e1b03cdf11
sprint_title: 'Fix the 2026-10-23 findings: satellite login/superadmin security, web console multi-satellite UX, fleet downgrade trap, Windows agent bugs, weave/sprint tooling, outpost headless/version skew'
closed: 2026-10-24T07:45:41.303491Z
closed_by: s497-manager
---

Found 2026-10-23: 'XDG_CONFIG_HOME=<scratch> XDG_CACHE_HOME=<scratch> outpost register --server <sat> --code ... --name <client>' on macOS printed 'replaced a prior outpost registration that this command manages / registered launchd agent io.dhnt.outpost (--user)' and rewrote ~/Library/LaunchAgents/io.dhnt.outpost.plist, which belongs to the real install. No harm this time because the plist carries no XDG env, but a scratch or second pairing must never touch the host's service. Expected: register installs or touches the service only for the default config dir, or with an explicit --service.

## Worker brief (s497-manager, 2026-10-24). One repo: outpost.
Rule: register/move install or touch the host service (launchd io.dhnt.outpost, systemd unit, Windows task) only when the resolved config dir is the DEFAULT one (reuse the resolved-vs-default comparison now in internal/agent/upgrade/worker.go from story b1037698 — factor it into internal/agent/conf if that avoids duplication), or when an explicit --service flag is passed. Otherwise print that the service was left untouched and how to run the daemon. Tests: temp HOME/XDG, a fake service installer seam; assert no service call for a scratch XDG dir, a call for default, a call with --service. Never touch real LaunchAgents/systemd in tests. Gate: go test -short -tags fb_archives ./cmd/outpost/ + touched packages, go build linux/darwin/windows. Trailers for this story only. No hostnames.
