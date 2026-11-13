---
id: b7133dae9e69
kind: bug
title: 'outpost ssh: named aliases always relay via cloudbox (no mesh rung); ad-hoc path waits ~3 s on mDNS before an already-live mesh link'
seq: 2773
status: todo
priority: p0
labels:
    - ssh
    - mesh
    - cost
created: 2026-11-13T04:36:57.291311Z
sprint: 629
sprint_id: c0fa6bb6-4b60-56b9-aac0-c267db8e1a18
sprint_title: 'outpost ssh: same-LAN peers go peer-to-peer (mesh-direct), not via the paid cloudbox relay'
---

Measured 2026-11-13 dev box -> Windows test host on the same LAN (outpost peers status: same_lan; outpost reach: route=mesh, link=lan, rtt 7 ms).
(1) Named alias `outpost ssh exec <alias>`: during the call the local daemon opened a NEW TCP connection to the cloudbox satellite endpoint (a DigitalOcean droplet) and none to the LAN peer; 1.4-2.6 s per no-op. cmd/outpost/ssh_dial.go dialSSHTargetChain only has cloudbox WS or plain-TCP Direct legs — no LAN/mesh rung — so every named-alias call is relayed: paid cloud bandwidth for same-room traffic, and extra latency.
(2) Ad-hoc `outpost ssh user@host`: no cloud connection observed (direct), but 3.4-4.2 s per call because dialLANDirect's mDNS browse blocks ~3 s before dialMeshDirect is tried (cmd/outpost/ssh_runtime.go ladder LAN -> mesh -> cloudbox). An agent run making ~30 remote calls lost ~2 min to this.
Classification: product defect (transport selection), not venue. Fix: share one ladder for named and ad-hoc targets; when peerstatus already reports a live DIRECT mesh link for the host, try mesh first and skip mDNS; keep cloudbox as fallback and log which rung carried the call (so relayed traffic is visible). Later: reuse a connection across calls (ControlMaster-like) for agents' many small commands. Verify: unit test of rung order with injected link state; live: named-alias exec to a same-LAN peer opens no cloudbox connection and completes < 1 s; cloudbox fallback still works when mesh is down.

## Delivery evidence (2026-11-13)

- Code: outpost 0b49c50 (mesh-direct rung: daemon exec path, named-alias
  dial, ad-hoc ladder mesh-first, shared peerticket package) + af7f4b2
  (review fixes: in-rung handshake with relay fallback, resolve 2s / ticket
  4s budgets, single close). Umbrella pin 5eca4bde.
- Independent review: 1 major (late mesh handshake failure did not fall
  back to the relay) + 3 minor, all fixed in af7f4b2 with regression tests.
- Unit: admincore 7 mesh-rung tests (no/relayed link, unknown host,
  unpublished peer, no cookie, forward released on failure, hung resolve
  bows out within 2s, failed mesh client releases forward); admincore and
  peerticket packages green; go vet clean.
- Targeted cmd/outpost (PeerTicket|SSH|Mesh|LAN|Dial|Reach|Connect|Sftp|
  Tunnel): 20/21 green. The single failure was the first run on 0b49c50,
  output not captured, not reproduced in 20 later runs (12 verbose).
  Cause UNKNOWN (suspected timing under host load) — recorded, not fixed.
- Live, dev box daemon upgraded to af7f4b2 (outpost upgrade --local) after
  the active weave run finished:
  - `outpost ssh exec elephant`: 0.66-0.69 s (relay) -> 0.31-0.45 s; daemon
    log `ssh: outer leg connected host=elephant route=mesh-direct`.
  - bulk exec: daemon's cloudbox connections carried ~38 KB total
    (keepalive + ticket) while the 1 MiB exec output arrived — data not
    relayed. (exec output is capped at 1 MiB.)
  - ad-hoc `outpost ssh liqiang@elephant`: 3.4-4.2 s -> ~0.4 s; a 20 MB pull
    moved 6.7 MB over the local mesh forward vs 5.5 KB/2.4 KB on the
    cloudbox connection (the ticket).
- Remaining: other hosts' daemons pick this up on their next upgrade
  (only the DIALING side needs it; peers already publish ssh on the mesh).
