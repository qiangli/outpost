---
id: 7c46f58d22eb
kind: task
title: generated LaunchDaemon plist omits PATH, so cluster mode=agent can never find podman
seq: 7
status: todo
priority: p2
created: 2026-07-22T08:54:47.905027Z
---

renderLaunchDaemonPlist sets only HOME in EnvironmentVariables. launchd's default PATH for daemons is /usr/bin:/bin:/usr/sbin:/sbin — it excludes /opt/homebrew/bin and /usr/local/bin, so a Homebrew- or bashy-installed podman is invisible to the daemon.

Effect: cluster mode=agent fails with

  WARN cluster mode=agent: runtime: no `podman` or `docker` binary on PATH
       (install Docker Desktop / Rancher Desktop / podman to enable --cluster-mode=agent)

even though `which podman` on the host resolves fine. runtime.pickPodmanBin looks up bare 'podman'/'docker' on PATH, and runtime.Options.PodmanBin is not wired to any config or env var, so there is no way to point it at an absolute path either.

Observed 2026-07-22 on novicortex: /opt/homebrew/bin/podman present, daemon could not see it. The host previously ran a HAND-ROLLED plist that set
  PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:/opt/homebrew/bin
and that PATH was silently dropped when the service was reinstalled from the generated template — the same reinstall that (correctly) migrated admin_addr/admin_users into agent.json. PATH is process environment, not app config, so it belongs in the template.

Fix: emit a PATH in renderLaunchDaemonPlist (and the LaunchAgent + systemd renders) covering the usual package-manager prefixes. Optionally also wire Options.PodmanBin to a config field so an absolute path can be pinned without touching the service definition.

Note this is what forced a plist hand-edit on a remote host, which is how 28b348e1 (self-severing reload) got triggered.
