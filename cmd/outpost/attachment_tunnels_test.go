package main

import (
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
)

func TestHostTunnelTargetsTwoAttachments(t *testing.T) {
	fc := &conf.FileConfig{Attachments: []conf.SatelliteAttachment{
		{SatelliteID: "one", Endpoint: "https://one.example", Tunnel: &conf.AttachmentTunnel{AgentName: "host", ServerAddr: "one.example", ServerPort: 443, Token: "one-token", RemotePort: 7001}},
		{SatelliteID: "two", Endpoint: "https://two.example", Tunnel: &conf.AttachmentTunnel{AgentName: "host", ServerAddr: "two.example", ServerPort: 8443, Token: "two-token", RemotePort: 7002}},
	}}
	if !fc.ApplyAttachmentTunnel() {
		t.Fatal("first attachment did not supply host identity")
	}
	targets := hostTunnelTargets(fc, &conf.Config{})
	if len(targets) != 2 || targets[0].config.ServerAddr != "one.example" || targets[1].config.ServerAddr != "two.example" || targets[0].remotePort != 7001 || targets[1].remotePort != 7002 || targets[1].config.Token != "two-token" {
		t.Fatalf("targets = %+v", targets)
	}
}
