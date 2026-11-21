package main

import (
	"time"

	"github.com/qiangli/outpost/internal/agent/admincore"
	"github.com/qiangli/outpost/internal/agent/heartbeat"
)

func heartbeatHealth(view admincore.ServiceStatusView, now time.Time) heartbeat.Reachability {
	health := heartbeat.Reachability{LANSSH: "unknown", Apps: "unknown", Power: "awake", PowerAt: now}
	if !view.MonitorReachable {
		return health
	}
	apps, active := 0, 0
	for _, unit := range view.Units {
		status := "unknown"
		switch {
		case !unit.Enabled:
			status = "disabled"
		case unit.State == "active":
			status = "up"
		case unit.State == "degraded" || unit.State == "failed" || unit.State == "inactive":
			status = "down"
		}
		if unit.Capability == "outpost.sshd" {
			health.LANSSH = status
		}
		if unit.App == "" || !unit.Enabled {
			continue
		}
		apps++
		switch status {
		case "down":
			health.Apps = "down"
		case "up":
			active++
		}
	}
	if apps > 0 && active == apps {
		health.Apps = "up"
	}
	return health
}
