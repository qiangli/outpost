package admincore

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/runtime"
)

func TestControlPlaneProbe_HostedRuntimeAbsentIsFailed(t *testing.T) {
	p := &defaultControlPlaneStatusProber{
		agentName:           "h1",
		controlPlaneEnabled: func() bool { return true },
		joinEndpoint:        func() string { return "" },
		health: func() (runtime.ServerHealth, bool) {
			return runtime.ServerHealth{Err: "connection refused"}, true
		},
	}
	st, err := p.ProbeControlPlaneStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.State != "failed" || !strings.Contains(st.FailureReason, "hosted but runtime absent: h1-control-plane") {
		t.Fatalf("state=%q reason=%q", st.State, st.FailureReason)
	}
	if !st.Hosted {
		t.Fatal("hosted flag must be left as is")
	}
	if got := cpStatusSummary(&st); !strings.HasPrefix(got, "FAILED: hosted but runtime absent") {
		t.Fatalf("summary %q", got)
	}
}

func TestBuildClusterPlaneView(t *testing.T) {
	now := time.Now()
	fc := &conf.FileConfig{Cluster: &conf.ClusterConfig{Enabled: ptrBool(true), APIURL: "https://user:pw@plane.example:6443/x?token=abc"}}
	h := runtime.PlaneHealth{LastSuccess: now.Add(-time.Hour), LastError: "ca fetch failed", LastErrorAt: now.Add(-time.Second)}
	v := BuildClusterPlaneView(fc, h, now)
	if v == nil || v.APIReachable != "no" || v.LastError != "ca fetch failed" || v.LastSuccess.IsZero() {
		t.Fatalf("%+v", v)
	}
	if strings.Contains(v.Endpoint, "pw") || strings.Contains(v.Endpoint, "abc") {
		t.Fatalf("endpoint leaks secrets: %q", v.Endpoint)
	}
	if BuildClusterPlaneView(&conf.FileConfig{}, h, now) != nil {
		t.Fatal("cluster off must yield no row")
	}
}

func ptrBool(b bool) *bool { return &b }
