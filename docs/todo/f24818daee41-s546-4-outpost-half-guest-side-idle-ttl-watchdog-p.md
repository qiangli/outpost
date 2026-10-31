---
id: f24818daee41
kind: feature
title: 'S546.4 (outpost half): guest-side idle/TTL watchdog powers off a provisioned GPU worker'
seq: 2763
status: todo
priority: p1
created: 2026-10-31T02:34:12.766207Z
sprint: 546
sprint_id: ae111a75-889f-50a4-a423-de1bab0fbca6
sprint_title: On-demand cloud GPU workers for large/custom Ollama models via Cloudbox and Outpost (AWS EC2 first)
---

Outpost half of S546.4 (676d853600b1), split only because work runs are per repo. Purpose: a lost control plane must never leave a GPU running. Second, independent idle stop beside the master's keep-alive stop. Active ONLY when OUTPOST_GPU_WORKER_REQUEST_ID is set (the provisioned GPU worker marker added in 59be587); every other host unchanged. Two triggers: (a) idle: no model loaded / no inference for OUTPOST_GPU_WORKER_IDLE (default 20m, deliberately longer than the master's warm window so the master's stop normally wins; reuse the ollama watcher's loaded-model probe, which already sees /api/ps), (b) hard cap: uptime past OUTPOST_GPU_WORKER_MAX_SESSION (default 4h) regardless of load. On trigger: log the reason, push one final registry update if possible, then power the host off via the OS (the instance is launched with shutdown behavior = stop, so poweroff STOPS, never terminates). Power-off goes through one injectable function so tests never power anything off; production uses the Linux reboot syscall power-off (or the system's poweroff), fail loud if not permitted. Document both env vars in docs/settings.md (+ embedded copy). Red first: fake clock + fake loaded-model source + fake poweroff; cases: non-worker host never arms; idle fires after threshold; activity resets idle; TTL fires even when busy; poweroff error is logged and retried, not swallowed. Gate: go test -short ./internal/agent/... (the package you touch) + go vet + GOOS=linux build of cmd/outpost. OSS: public wire shape only; no hostnames, ids. Bashy-only mechanisms.
