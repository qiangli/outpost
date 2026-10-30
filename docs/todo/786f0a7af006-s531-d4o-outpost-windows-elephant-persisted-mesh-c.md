---
id: 786f0a7af006
kind: task
title: 'S531.D4o outpost (Windows elephant): persisted mesh consumes listed but not listening after restart'
seq: 2760
status: done
priority: p0
created: 2026-10-29T17:58:00.276776Z
assignee: muse-conductor
sprint: 531
sprint_id: f7bc2570-ca8e-59b0-8163-c3c51ba23610
sprint_title: 'Genie core harness: drive vendor agent CLIs as tools'
closed: 2026-10-29T22:27:10.33105Z
closed_by: s531-manager
---

Found in D4 live 10-29 on elephant (Windows, outpost v1.0.0-rc.22-dev). 'outpost mesh consume ls' listed llm-door -> 127.0.0.1:49940 and -> 172.27.208.1:24556, but both refused connections (curl exit 7). rm + add of the same consume (MeshConsumeUpsert -> MeshForward.Listen) brought them live at once (the door answered 401). So the boot-time re-establish of conf.MeshConsumes is lost or fails silently, e.g. when the peer is not yet connected at boot, the bind address (WSL vEthernet) is not up yet, or the forward dies after a peer reconnect. Fix: re-establish persisted consumes with retry/backoff until they bind, re-bind after a listener error, and show live/not-live per consume in 'consume ls'. Red/green unit test at the MeshForward/admincore seam; go test -short on touched packages only (no full suite on Dragon).
