---
id: de97f6bdfd9b
kind: task
title: 'design: make service install safe by construction, not by guard rails'
seq: 9
status: todo
priority: p1
created: 2026-07-22T09:13:37.508634Z
---

DESIGN TASK — do not patch ad-hoc. `outpost service install` is privileged, destructive, and can sever its own transport. It needs a deliberate design, not another flag.

## What happens today

installService writes the rendered definition with an unconditional
os.WriteFile(path, plist) — no Stat, no backup, no diff, no prompt. Whatever was
there is gone. Its only caller is the explicit command (upgrades never
regenerate the definition; they swap the binary only), which is precisely why
hand-rolled plists survived years of reboots, relocations and version pushes
across this fleet — until someone ran install once.

## Root cause: two sources of truth

The definition is generated from a template, but real hosts carry operator
customizations that exist ONLY in the plist. Any 'preserve the edits' mechanism
(backup, merge, diff-and-warn) makes that split permanent instead of removing
it.

Evidence from the fleet on 2026-07-22:
  - novidesign  hand-rolled plist: PATH + OUTPOST_ADMIN_ADDR. Its LAN admin bind
                comes ENTIRELY from that env var — agent.json has no admin_addr,
                and `config show` reports the default while the daemon listens
                on *:17777.
  - novicortex  same shape, until install regenerated it. Lost PATH; cluster
                mode=agent then could not find podman.
  - dragon      no service registration at all; runs from a shell and inherits
                an interactive PATH.

So NO host ran the generated template. The installer was effectively untested
until its first real use, which caused an outage.

## The direction

Make the generated definition SUFFICIENT, so nothing needs hand editing and
regenerating loses nothing:
  - PATH — done in 90e5063.
  - admin_addr / admin_users — config fields already exist; novidesign is still
    env-only and should be migrated.
  - anything else operators customize should get a config field rather than a
    plist edit.

Then install is a pure function of (binary, user, home) + agent.json, and is
idempotent and safe BY CONSTRUCTION rather than by guard rail.

## Second, independent hazard: it can sever itself

Reloading the definition means launchctl bootout+bootstrap, but `ssh <host>`
lands in outpost's own in-process shell — so bootout kills the process running
the command and the bootstrap never executes. The host is left with the service
UNLOADED and unreachable until someone reboots it or has physical access. That
is what happened to novicortex. See 28b348e1.

## Threat model to design against

An agent (or a person skim-reading docs) runs `outpost service install` on a
remote host because it looks routine. Today that can: silently discard operator
config, and permanently disconnect the host. Neither is recoverable remotely.

Worth considering: dry-run as the DEFAULT posture with an explicit apply;
refusing when the operation would sever the transport it is running over;
making the write idempotent (no-op when the rendered definition already
matches); and treating 'definition differs from template' as a condition to
report rather than to silently resolve.

Related: 7c46f58d (PATH), d7bb762e (MCP cluster_mode), 28b348e1 (self-severing
reload).
