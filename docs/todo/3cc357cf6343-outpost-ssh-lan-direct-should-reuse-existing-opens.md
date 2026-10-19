---
id: 3cc357cf6343
kind: enhancement
title: outpost ssh LAN-direct should reuse existing OpenSSH key trust instead of requiring elevation or outpost sshd
seq: 1898
status: todo
labels:
    - ssh
created: 2026-10-19T02:19:42.657136Z
---

Observed 2026-10-18 (Sprint 446 S16/S18 peer legs): three LAN hosts already have passwordless OpenSSH (port 22, ~/.ssh keys) between them, yet outpost ssh could not reach a peer. The alias route (outpost ssh exec <alias>) required a cloudbox elevation token (elevation required ... run outpost connect <host>), and the ad-hoc LAN route (outpost ssh user@host.local) dialed outpost's own sshd on :2222, which was not running (connection refused). Headless agents cannot answer the elevation password prompt, so peer automation stalls although LAN key trust exists. Expected: on a LAN peer, outpost ssh/sftp can use the host's existing OpenSSH key trust (or an outpost sshd started with an authorized-keys file) without cloudbox elevation, and the error names the route it tried. Workaround: operator runs outpost connect <host> once.
