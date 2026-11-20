package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/service"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/otel"
)

func TestPodmanAbsentSocketStaysQuietThenActivates(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "pm")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "p.sock")

	target := agent.BuiltinTarget{Name: "podman", Scheme: "unix", Socket: sock}
	apps := agent.NewAppRegistry()
	c := &podmanCapability{Apps: apps, Detect: func() agent.BuiltinTarget { return target }}
	ctx := context.Background()

	if err := c.Activate(ctx, service.Config{}); err != nil {
		t.Fatalf("absent socket must not fail activation: %v", err)
	}
	if r := c.Unavailable(); !strings.Contains(r, "unavailable") || !strings.Contains(r, sock) {
		t.Errorf("reason should name what was probed, got %q", r)
	}
	if len(apps.Names()) != 0 {
		t.Error("nothing may be published while unavailable")
	}
	if err := c.Health(ctx); err != nil {
		t.Errorf("Health while absent must not fail: %v", err)
	}

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	target.Available = true
	if err := c.Health(ctx); err != nil {
		t.Fatalf("Health after socket appeared: %v", err)
	}
	if c.Unavailable() != "" {
		t.Errorf("still unavailable: %q", c.Unavailable())
	}
	if len(apps.Names()) == 0 {
		t.Error("podman app not published after the socket appeared")
	}
}

// TestPodmanAbsentSocketRestsInactiveThroughProvider is the
// 9deb591a regression at the layer operators read: driven through
// service.Internal (what the daemon's owner loop reports from), an
// absent podman socket must rest Inactive with the unavailable
// reason — never Active/health ok, which hid a 39 h outage — and
// recover to Active on the Activate after the socket appears.
func TestPodmanAbsentSocketRestsInactiveThroughProvider(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "pm")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "p.sock")

	target := agent.BuiltinTarget{Name: "podman", Scheme: "unix", Socket: sock}
	apps := agent.NewAppRegistry()
	c := &podmanCapability{Apps: apps, Detect: func() agent.BuiltinTarget { return target }}
	reg := service.NewRegistry()
	if err := reg.Register(capPodman, c); err != nil {
		t.Fatal(err)
	}
	p := service.NewInternal("outpost", reg)
	unit := fleet.Service{Name: "podman", Kind: fleet.KindService, Provider: fleet.ServiceProviderInternal,
		Capability: capPodman, Owner: "outpost", Enabled: true}
	ctx := context.Background()

	if err := p.Activate(ctx, unit, service.Runtime{}); err != nil {
		t.Fatalf("absent socket must not fail activation: %v", err)
	}
	st := p.Status("podman")
	if st.State != service.Inactive || st.State.Exposed() {
		t.Fatalf("status = %+v, want resting Inactive (was Active before the fix)", st)
	}
	if !service.IsUnavailableReason(st.Reason) || !strings.Contains(st.Reason, sock) {
		t.Fatalf("reason = %q, want unavailable naming the probed socket", st.Reason)
	}
	if err := p.Health(ctx, "podman"); err == nil || !strings.Contains(err.Error(), "no podman socket detected") {
		t.Fatalf("provider health while absent = %v, want the unavailable reason", err)
	}

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	target.Available = true
	if err := p.Activate(ctx, unit, service.Runtime{}); err != nil {
		t.Fatalf("activate after socket appeared: %v", err)
	}
	if st := p.Status("podman"); st.State != service.Active || st.Reason != "" {
		t.Fatalf("status after socket appeared = %+v, want Active", st)
	}
	if len(apps.Names()) == 0 {
		t.Error("podman app not published after the socket appeared")
	}
	if err := p.Health(ctx, "podman"); err != nil {
		t.Fatalf("provider health after socket appeared: %v", err)
	}
}

func TestOtelAbsentProxyStaysQuietThenActivates(t *testing.T) {
	target := otel.Target{ManifestPath: "/nonexistent/manifest.json"}
	apps := agent.NewAppRegistry()
	c := &otelCapability{Apps: apps, Detect: func() otel.Target { return target }}
	ctx := context.Background()
	if err := c.Activate(ctx, service.Config{}); err != nil {
		t.Fatalf("absent proxy must not fail activation: %v", err)
	}
	if !strings.Contains(c.Unavailable(), "/nonexistent/manifest.json") {
		t.Errorf("reason = %q", c.Unavailable())
	}
	if err := c.Health(ctx); err != nil {
		t.Fatalf("Health while absent: %v", err)
	}
	target = otel.Target{Available: true, ProxyURL: "http://127.0.0.1:1", Token: "t", ManifestPath: "m"}
	if err := c.Health(ctx); err != nil {
		t.Fatalf("Health after proxy appeared: %v", err)
	}
	if c.Unavailable() != "" || len(c.registered) == 0 {
		t.Errorf("not activated: reason=%q registered=%v", c.Unavailable(), c.registered)
	}
}

func TestWarmUnpairedStaysQuiet(t *testing.T) {
	paired := false
	c := &warmCapability{Paired: func() bool { return paired }}
	if err := c.Activate(context.Background(), service.Config{}); err != nil {
		t.Fatalf("unpaired warm must not fail: %v", err)
	}
	if c.Unavailable() == "" {
		t.Error("expected unavailable reason")
	}
	if err := c.Health(context.Background()); err != nil {
		t.Errorf("Health: %v", err)
	}
}
