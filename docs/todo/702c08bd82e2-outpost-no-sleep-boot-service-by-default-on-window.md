---
id: 702c08bd82e2
kind: feature
title: 'outpost: no-sleep + boot service by default on Windows/macOS/Linux, and ''outpost verify --post-install'' (wake + reboot must-check)'
seq: 1901
status: done
priority: p0
created: 2026-10-23T19:29:23.626347Z
weave: 81
assignee: codex-gpt6-astra
sprint: 495
sprint_id: d7c905ee-3395-5fb3-82aa-b1585a02530e
sprint_title: 'Seamless onboarding: bashy onboard, setup-recipe rods, install/signing/winget'
closed: 2026-10-23T19:52:50.507611Z
resolution: fixed
closed_by: codex-gpt6-astra-c
---

Implementation half of yoke fddf853b (operator rule 2026-10-23). Repo: outpost ONLY. The bashy setup-recipe step that calls this is a separate story. Public: no hostnames or user ids.
1. `outpost service install` (all OSes) installs outpost (and outpost.sshd when configured) as a boot-surviving service that runs without a logged-in user:
   - Windows: a service or a Task Scheduler at-startup task (see service_windows.go);
   - macOS: a LaunchDaemon (or a LaunchAgent with RunAtLoad/KeepAlive, plus documentation of why);
   - Linux: a systemd user unit with `loginctl enable-linger`, or a system unit (see service_linux.go).
   Idempotent.
2. Power: disable sleep by default, opt-out with `--keep-sleep`.
   - Windows: powercfg standby/hibernate AC+DC = 0, `powercfg /hibernate off`.
   - macOS: `pmset -a sleep 0 disksleep 0 womp 1`.
   - Linux: mask sleep/suspend/hibernate/hybrid-sleep targets.
   When admin rights are missing, print the exact command to run and exit non-zero for that step, never silently.
3. `outpost verify --post-install [--reboot]`:
   - reads back the power settings (sleep disabled) and the service state (boot-enabled);
   - with `--reboot`, schedules a reboot and, on the next start, records that outpost came back online on its server without a login;
   - writes PASS/FAIL JSON with the raw readbacks under the outpost cache dir;
   - `outpost doctor` (or status) shows sleep: disabled|enabled and service: boot|session|none.
Acceptance, red/green: unit tests per OS for the command builders and readback parsing (table tests; fakes for powercfg/pmset/systemctl output); go build ./... for GOOS=windows,darwin,linux; go vet. Live: the manager verifies on the three approved hosts (macOS, Windows, Linux) including one reboot each on non-production hosts.
