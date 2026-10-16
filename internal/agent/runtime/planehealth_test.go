package runtime

import (
	"testing"
	"time"
)

func TestPlaneHealth_LogFailureThenProbeSuccess(t *testing.T) {
	ResetPlaneHealth()
	now := time.Now()
	if LastPlaneHealth().Reachable(now) != "unknown" {
		t.Fatal("no signal must be unknown")
	}
	RecordPlaneLogLine("frpc: CA fetch failed: connection refused", now)
	RecordPlaneLogLine("k3s: starting kubelet", now)
	h := LastPlaneHealth()
	if h.Reachable(now) != "no" || h.LastError == "" {
		t.Fatalf("%+v", h)
	}
	RecordPlaneProbe(ServerHealth{Serving: true, CheckedAt: now.Add(time.Second)})
	if got := LastPlaneHealth().Reachable(now.Add(2 * time.Second)); got != "yes" {
		t.Fatalf("after success: %s", got)
	}
}
