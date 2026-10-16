package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
