---
id: b10376985cf9
kind: bug
title: a second outpost started from an ssh session inherits OUTPOST_SUPERVISED=1 and exits on its restart path with no supervisor; with a shared binary path its upgrade puller can swap the main daemon's binary
seq: 1904
status: done
priority: p2
created: 2026-10-23T20:17:51.077147Z
assignee: muse-spark1.2
sprint: 497
sprint_id: 16af0380-7ec7-5d41-a9c6-50e1b03cdf11
sprint_title: 'Fix the 2026-10-23 findings: satellite login/superadmin security, web console multi-satellite UX, fleet downgrade trap, Windows agent bugs, weave/sprint tooling, outpost headless/version skew'
closed: 2026-10-24T07:17:24.494835Z
closed_by: s497-manager
---

Found 2026-10-23 on a Linux host: an outpost started from a shell spawned by the supervised main outpost inherited OUTPOST_SUPERVISED=1. When its upgrade puller accepted the fleet target, execSelfStart did os.Exit(0) ('supervised - exiting for the supervisor to respawn') and the instance was gone. Workaround: unset OUTPOST_SUPERVISED and run from a private binary copy. Expected: OUTPOST_SUPERVISED is stripped from shell/exec sessions served by the daemon (or the supervisor passes a token that a child cannot inherit), and a non-default config dir never self-upgrades a binary shared with another instance.

## Rework 1 (s497-manager, 2026-10-24). Salvage of run #86: refs/salvage/abandoned-86 (commit b10513e).
Keep the BuildEnv strip of OUTPOST_SUPERVISED (good), but fix and test:
1. The upgrade guard must NOT key on `XDG_CONFIG_HOME != ""`. Many main installs export XDG_CONFIG_HOME=~/.config; they must keep self-upgrading. Compare the RESOLVED config dir the daemon actually uses with the DEFAULT config dir (the one used with no override, e.g. os.UserConfigDir()/outpost or the existing conf helper). Only a non-default config dir AND a binary path outside that config tree refuses self-upgrade.
2. Tests (red/green, isolated temp HOME/XDG, no real paths, no self-exec):
   - internal/agent/shell: BuildEnvWith strips OUTPOST_SUPERVISED from inherited env and from overrides; other vars kept.
   - internal/agent/upgrade: table test for the guard: (a) XDG unset -> allowed; (b) XDG set to the default dir -> allowed; (c) XDG set to a scratch dir + binary outside it -> refused (StatusDisabled, clear Detail); (d) scratch dir + binary inside it -> allowed.
3. Gate: go test -short ./internal/agent/upgrade/... ./internal/agent/shell/ -run 'Env|Guard|Shared|Supervised' (shell tests may need a TTY: keep the new tests TTY-free), go build ./... for linux/darwin/windows.
