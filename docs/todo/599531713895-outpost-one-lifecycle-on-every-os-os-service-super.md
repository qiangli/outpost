---
id: "599531713895"
kind: enhancement
title: 'outpost: one lifecycle on every OS (OS service -> supervisor -> daemon), stop-intent vs kill, remove behavior toggles, same verify --post-install (9a496a1e)'
seq: 1910
status: done
priority: p1
created: 2026-10-24T19:12:38.370668Z
assignee: claude-sonnet5.5
sprint: 497
sprint_id: 16af0380-7ec7-5d41-a9c6-50e1b03cdf11
sprint_title: 'Fix the 2026-10-23 findings: satellite login/superadmin security, web console multi-satellite UX, fleet downgrade trap, Windows agent bugs, weave/sprint tooling, outpost headless/version skew'
closed: 2026-10-25T10:49:40.502695Z
closed_by: s497-manager
---

Outpost side of umbrella story 9a496a1e (operator 2026-10-24). Design of record: umbrella docs/outpost-robustness-design.md sections 1-3 (read it). One repo: outpost. Base: main 74ae98e (P0 never-older guard + always-on auto-rollback already merged).
Deliver (this story = sections 1 and 3 + the option audit; A/B slots, safe mode and the rescue slot are follow-up stories):
1. One lifecycle on every OS: OS service (launchd KeepAlive / systemd Restart=always / Windows Task Scheduler S4U with restart-on-failure) starts the bashy supervisor at boot with no login; the supervisor runs and restarts the daemon; restart and upgrade always go through the supervisor. Same code path on all three; only the OS-specific install files differ. Find and remove any OS-specific behavior forks.
2. Stop vs kill: only `outpost stop` writes a stop-intent marker; the supervisor restarts the daemon after any other exit (kill -9, crash) with backoff; the intent clears at boot (unless `outpost disable`).
3. Option audit: remove behavior toggles that change HOW outpost runs (update-mode/auto-upgrade style flags, legacy supervision switches, anything like the removed --auto-rollback); keep only sharing/access/security options. List what you removed in the commit body. No deprecation paths.
4. `outpost verify --post-install` runs the same checks on macOS/Linux/Windows (service installed + enabled at boot, supervisor running, daemon healthy, sshd listening, no sleep/hibernate on always-on hosts, PATH sane in the boot session).
Tests: unit tests per OS for the generated service definitions (string/template level), stop-intent vs kill behavior with a fake process, option removal. Temp HOME/XDG only; never install real services. No hostnames. Gate: go test -short -tags fb_archives ./cmd/outpost/ + touched packages, go build ./... linux/darwin/windows. Commit with this story's trailers only.

## Rework 1 (s497-manager, 2026-10-24). Run #92 (commit 3505241) content approved in review, but it was based on 8fd1214 and conflicts in 12 files with what merged since: outpost main 74ae98e (P0 1119b8f0: shared never-older guard, always-on auto-rollback, flag/config removal, S4U boot chain tests) and d06474c/d569660 (936549d4: register/recover touch the host service only for the DEFAULT config dir or --service; conf.IsDefaultConfigDir / ShouldTouchHostService). Rebase 3505241 onto current outpost main (74ae98e) and resolve so that BOTH merged fixes survive unchanged in intent:
- keep the P0 guard + always-on rollback exactly as merged (do not reintroduce removed flags);
- KEEP the 936549d4 rule: a scratch/non-default config dir never installs or touches the host service (your removal of `register --no-service` is fine, but default-dir gating and the explicit --service stay);
- your lifecycle, stop-intent/disable, option audit and verify --post-install changes on top.
Tests must include the existing register_service_gate_test.go, worker_downgrade_test.go, github_newest_test.go, supervisord_watchdog_test.go. Gate: go test -short -tags fb_archives ./cmd/outpost/ ./internal/agent/... (skip internal/agent/shell TTY tests) + go build ./... linux/darwin/windows. One commit with this story's trailers.

## Rework 2 (s497-manager, 2026-10-25). Run #93 (1d53406) was NOT rebased (still on 8fd1214) and against main it reverts parts of the merged P0 (worker_downgrade_test.go/worker_test.go shrink, guard code drifts) and adds 3 real host names. New worker. Do this:
1. Your workspace starts at current outpost main (>= 74ae98e). Do NOT cherry-pick 1d53406 wholesale. Use it only as a reference (`git show 1d53406`, available via refs/salvage/abandoned-93) and re-apply its INTENT on top of main: systemd Restart=always; stop-intent marker + `outpost disable`/`enable`; option audit (remove update-mode/auto-upgrade/never-manual gates, register --no-service) WITHOUT touching the merged P0 guard/rollback code or the 936549d4 default-config-dir service gating; `verify --post-install` six checks on all three OSes.
2. Before committing, prove nothing merged regressed: `git diff origin/main -- internal/agent/upgrade/worker_downgrade_test.go internal/agent/upgrade/github_newest_test.go cmd/outpost/register_service_gate_test.go` must show ONLY additions (no deleted test cases).
3. Public repo: no real host names (`git diff origin/main | grep -niE 'elephant|lern|ivy|alien|novi|dragon'` empty).
Gate: go test -short -tags fb_archives ./cmd/outpost/ ./internal/agent/upgrade/... ./internal/agent/conf/ ./internal/agent/admincore/ + go build ./... linux/darwin/windows. One commit with this story's trailers.
