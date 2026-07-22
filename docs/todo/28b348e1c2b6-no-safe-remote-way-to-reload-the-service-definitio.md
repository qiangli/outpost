---
id: 28b348e1c2b6
kind: task
title: no safe remote way to reload the service definition — launchctl over outpost's own shell severs itself
seq: 6
status: todo
priority: p1
created: 2026-07-22T08:54:31.101713Z
---

Changing the service DEFINITION (plist/unit) requires launchctl/systemctl. But 'ssh <host>' lands in outpost's IN-PROCESS shell ('Matrix shell (qiangli/sh in-process)'), so 'launchctl bootout system/io.dhnt.outpost' kills the process hosting the command — every statement after it dies with the connection. The reload never runs and the host is left with the service UNLOADED, i.e. unreachable until someone reboots it or has local access.

Happened 2026-07-22 on novicortex, a remote host with no LAN route from the operator's current network. It stayed down for days. 'outpost restart' is safe (daemon re-exec under supervisord); it is only the service-level reload that has no safe path — and nothing warns you.

What makes it worse: there is a legitimate reason to edit the plist (see the PATH task), so operators WILL end up here.

Wanted: an 'outpost service reload' that survives losing the connection — detach the bootout+bootstrap pair (double-fork / launchd-submitted one-shot / 'at'-style job) so the reload completes even when the caller's shell dies. It should also refuse, or loudly warn, when it detects it is running inside outpost's own shell on the host it is about to unload.

Recovery for the record:
  ssh <host-lan-ip> 'sudo launchctl bootstrap system /Library/LaunchDaemons/io.dhnt.outpost.plist'
via the host's REAL sshd, which is independent of outpost — but only reachable from its LAN.
