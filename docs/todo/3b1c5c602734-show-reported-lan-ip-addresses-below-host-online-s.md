---
id: 3b1c5c602734
kind: task
title: Show reported LAN IP addresses below host online status
seq: 2764
status: todo
created: 2026-11-04T23:39:05.62653Z
sprint: 569
sprint_id: eb2bc5c7-797d-5001-b378-eeba0fb485aa
sprint_title: Show Outpost LAN addresses beside host online status
---

Report optional system.lan_ips for the host-list LAN address display requested by the operator. Collect private IPv4 and IPv6 addresses from active non-loopback interfaces on each system poll, sort and deduplicate, and omit unavailable values. No listener or access-control changes. Verification: go test ./internal/agent/sysinfo passed; git diff --check passed. Test covers private IPv4/IPv6 and exclusion of loopback, link-local, public, unspecified and unsupported addresses. No binary release or deployment performed; independent acceptance remains pending.
