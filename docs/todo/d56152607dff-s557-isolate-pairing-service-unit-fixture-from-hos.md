---
id: d56152607dff
kind: bug
title: S557 isolate pairing service unit fixture from host config paths
seq: 2764
status: assigned
priority: p1
created: 2026-11-01T17:04:33.570314Z
assignee: s557-manager
sprint: 557
sprint_id: f5ccf563-f5d1-5b94-8be4-9388e51ef86d
sprint_title: 'Bashy v1.0.0 preparation: split private/public development and retire qiangli/dhnt'
---

Hosted native TestActivatePairedService assumes default config but inherits HOME/XDG paths from its process. Set temporary HOME and USERPROFILE plus empty XDG_CONFIG_HOME in this mocked service test. Preserve separate scratch/override service gate tests and production safety behavior; validate remotely because package TestMain builds full product.
