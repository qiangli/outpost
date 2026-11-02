---
id: d56152607dff
kind: bug
title: S557 isolate pairing service unit fixture from host config paths
seq: 2764
status: wontfix
priority: p1
labels:
    - superseded
created: 2026-11-01T17:04:33.570314Z
assignee: s557-manager
sprint: 557
sprint_id: f5ccf563-f5d1-5b94-8be4-9388e51ef86d
sprint_title: 'Bashy v1.0.0 preparation: split private/public development and retire qiangli/dhnt'
closed: 2026-11-02T05:32:11.104344Z
---

Hosted native TestActivatePairedService assumes default config but inherits HOME/XDG paths from its process. Set temporary HOME and USERPROFILE plus empty XDG_CONFIG_HOME in this mocked service test. Preserve separate scratch/override service gate tests and production safety behavior; validate remotely because package TestMain builds full product.

## Superseded by consolidation — 2026-11-02

Operator requested consolidation of implementation repairs into acceptance owners. Scope, original acceptance, unresolved work and evidence transfer to public umbrella #6, story `bf7624a5c2f2`. This record is closed by decision with label `superseded`; it is NOT a delivery acceptance, test pass, waiver or feature retirement. The parent remains open. Continue corrections under that parent rather than creating another repair story. Original content above is retained unchanged.
