---
id: 337dff3b3cfa
kind: bug
title: 'outpost: bump yoke/coreutils/gotreesitter pins incl. the yoke/pkg/provider replace (build break 450bad44)'
seq: 1908
status: done
priority: p1
created: 2026-10-24T06:07:33.440595Z
weave: 84
assignee: genie-glm-5.3
sprint: 497
sprint_id: 16af0380-7ec7-5d41-a9c6-50e1b03cdf11
sprint_title: 'Fix the 2026-10-23 findings: satellite login/superadmin security, web console multi-satellite UX, fleet downgrade trap, Windows agent bugs, weave/sprint tooling, outpost headless/version skew'
closed: 2026-10-24T06:50:52.133397Z
closed_by: s497-manager
---

Outpost side of umbrella story 450bad44 (Sprint 497). The 'godo mismatch' diagnosis was wrong: neither outpost nor yoke uses github.com/digitalocean/godo. Real cause: yoke/pkg/fleet/infra_firewall.go imports github.com/qiangli/yoke/pkg/provider/digitalocean, a SEPARATE nested module (yoke/pkg/provider/go.mod). outpost/go.mod pins it by a replace (line ~328: `replace github.com/qiangli/yoke/pkg/provider => ... v0.0.0-20261021020216-636e2fd289e9`; the require at ~219 is a placeholder), and `go get yoke@X yoke/pkg/provider@X` does not override that replace, so new pkg/fleet builds against the old provider (missing FirewallRule, RuleTargets, DefaultOutboundRules, ParsePortProtocol, AddRulesToFirewall from yoke pkg/provider/digitalocean/hardening.go, added in baac4c21/7c916cc2/6c044b84/e2d1d5aa).

## Worker brief (s497-manager). One repo: outpost.
Bump outpost to current yoke origin/main HEAD (and coreutils origin/main HEAD), updating together: the yoke require, the yoke/pkg/provider replace, the other stale yoke nested-module replaces (llmgw, oci ~322/324), go-billy (~330) if yoke's differs, and align the gotreesitter replace (~314) with yoke's (f03173f8). Use pseudo-versions that resolve from the public module proxy / GitHub (GOWORK=off). Then `GOWORK=off go mod tidy`, `GOWORK=off go build ./...` for GOOS=linux, darwin, windows (all must pass), and `GOWORK=off go test -short ./internal/agent/upgrade/... ./cmd/outpost/...` (skip internal/agent/shell: needs a TTY). Fix only build breaks caused by the API drift (minimal adapters). No feature changes. Trailers for this story only.
