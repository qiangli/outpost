package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/outpost/internal/agent/admincore"
)

func TestPrintControlPlaneStatus_FailedRuntimeAbsent(t *testing.T) {
	var buf bytes.Buffer
	printClusterStatus(&buf, admincore.StatusView{
		ControlPlaneState:   "failed",
		ControlPlaneFailure: "hosted but runtime absent: x-control-plane (container/podman probe found no such container)",
	})
	if !strings.Contains(buf.String(), "FAILED: hosted but runtime absent") {
		t.Fatalf("got %q", buf.String())
	}
}

func TestPrintClusterStatus_UnreachablePlane(t *testing.T) {
	var buf bytes.Buffer
	printClusterStatus(&buf, admincore.StatusView{Cluster: &admincore.ClusterPlaneView{
		Joined: true, Endpoint: "https://plane.example:6443", APIReachable: "no",
		LastSuccess: time.Unix(1700000000, 0), LastError: "ca fetch failed: refused",
	}})
	out := buf.String()
	for _, want := range []string{"apiserver_reachable=no", "plane.example", "last_success", "ca fetch failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
}
