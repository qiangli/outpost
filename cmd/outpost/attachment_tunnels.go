package main

import (
	"log/slog"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/conf"
)

type hostTunnelTarget struct {
	config     agent.TunnelConfig
	remotePort int
}

// hostTunnelTargets makes one tunnel per valid attachment. The local HTTP
// listener and all host services are created once by the caller.
func hostTunnelTargets(fc *conf.FileConfig, cfg *conf.Config) []hostTunnelTarget {
	if fc != nil && len(fc.Attachments) > 0 && (!fc.LegacyPaired() || fc.AttachmentTunnelActive()) {
		var targets []hostTunnelTarget
		for _, a := range fc.Attachments {
			if a.Tunnel == nil || a.Tunnel.AgentName == "" || a.Tunnel.ServerAddr == "" {
				slog.Warn("matrix-agent: satellite attachment has no tunnel registration; re-run `outpost satellite attach`", "satellite", a.SatelliteID)
				continue
			}
			t := a.Tunnel
			targets = append(targets, hostTunnelTarget{config: agent.TunnelConfig{
				ServerAddr: t.ServerAddr, ServerPort: t.ServerPort,
				Protocol: t.Protocol, Token: t.Token, User: t.AgentName,
			}, remotePort: t.RemotePort})
		}
		return targets
	}
	return []hostTunnelTarget{{config: agent.TunnelConfig{
		ServerAddr: cfg.ServerAddr, ServerPort: cfg.ServerPort,
		Protocol: cfg.Protocol, Token: cfg.Token, User: cfg.AgentName,
	}, remotePort: cfg.RemotePort}}
}
