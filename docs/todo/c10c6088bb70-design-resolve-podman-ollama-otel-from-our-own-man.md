---
id: c10c6088bb70
kind: task
title: 'design: resolve podman/ollama/otel from our own managed cache, not $PATH'
seq: 10
status: todo
priority: p1
created: 2026-07-22T09:24:00.751894Z
---

DESIGN TASK. Today outpost finds its tool dependencies by name on PATH — runtime.pickPodmanBin tries opts.PodmanBin, then 'podman', then 'docker'. That makes a service definition's PATH load-bearing, which is how a remote host lost cluster mode=agent on 2026-07-22 ('no podman or docker binary on PATH' while 'which podman' resolved fine in a shell).

We should not depend on PATH for tools we already ship.

## We already have the cached copies

The bashy bin cache on dragon (binmgr.CacheDir() = $BASHY_BIN_CACHE, else <UserCacheDir>/bashy/bin) contains:

  act act_runner bashy doctl gh go gvproxy helm kubectl loom maven mise node
  ollama podman podman-remote rg rustup-init temurin-jdk-21 uv vfkit
  victoria-logs victoria-metrics victoria-traces

podman, ollama and the otel stack (victoria-*) are all present, plus podman's own machine deps (gvproxy, vfkit). dragon's PATH-visible podman is just a symlink into it:

  ~/bin/podman -> ~/Library/Caches/bashy/bin/podman

So the binaries are provisioned, verified (podman.sha256 sits beside it) and version-managed. outpost simply does not look there.

## The blocker: two layouts in one directory

  bin/bashy/v0.13.1/bashy     binmgr VERSIONED layout — what CachedBinary globs
                              (<root>/<name>/*/<binary>)
  bin/podman                  FLAT file placed by bashy's own provisioning
                              (+ podman.sha256 beside it)

binmgr.CachedBinary("podman") therefore returns "" today even though the binary is right there. outpost uses binmgr for exactly one tool (bashy, cmd/outpost/bashy.go) and PATH for everything else.

## Direction

Resolve managed tools through one accessor that understands where they actually live, and consult it BEFORE PATH:

  - podman  -> runtime.pickPodmanBin and agent.DetectPodman
  - ollama  -> the ollama proxy/watcher
  - otel    -> the victoria-* / observability built-ins

Decisions to make deliberately:
  - reconcile the two layouts, or teach the resolver both. One scheme is
    preferable; two is how this bug happened.
  - whether outpost should Ensure() a missing tool (download on demand) or only
    use what bashy already provisioned. Downloading from a boot service has its
    own failure modes.
  - keep PATH as a last-resort fallback for operator-installed tools, but it
    must stop being the primary path.

## Why this matters beyond tidiness

It removes PATH from the service definition's responsibilities entirely. The
PATH added to the launchd/systemd renders in 90e5063 stays useful for
operator tooling, but stops being load-bearing — and a regenerated service
definition can no longer cost a host its container runtime. Directly reduces
the blast radius described in de97f6bd.

Windows needs nothing today (podman.exe is in System32, verified on puppy), but
the same resolver would cover it uniformly rather than by luck of install
location.
