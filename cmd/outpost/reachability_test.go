package main

import (
	"testing"

	"github.com/qiangli/outpost/internal/agent/admincore"
)

func TestHeartbeatHealthUsesLiveServiceStatus(t *testing.T) {
	view := admincore.ServiceStatusView{MonitorReachable: true, Units: []admincore.ServiceUnitView{
		{Capability: "outpost.sshd", Enabled: true, State: "failed"},
		{App: "fixture", Enabled: true, State: "active"},
		{App: "disabled", Enabled: false, State: "failed"},
	}}
	if got := heartbeatHealth(view); got.LANSSH != "down" || got.Apps != "up" || got.Power != "" || !got.PowerAt.IsZero() {
		t.Fatalf("health: %+v", got)
	}
	view.Units[1].State = "degraded"
	if got := heartbeatHealth(view); got.Apps != "down" {
		t.Fatalf("degraded app health: %+v", got)
	}
	view.MonitorReachable = false
	if got := heartbeatHealth(view); got.LANSSH != "unknown" || got.Apps != "unknown" {
		t.Fatalf("stale health: %+v", got)
	}
}
