package main

import (
	"context"
	"testing"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/otel"
	"golang.org/x/sync/errgroup"
)

func TestOtelMetricsTargetLifecycle(t *testing.T) {
	t.Setenv("BASHY_OTEL_URL", "")
	c := &otelCapability{Apps: agent.NewAppRegistry(), Detect: func() otel.Target {
		return otel.Target{Available: true, ProxyURL: "http://collector.example", Token: "test-token"}
	}}
	if _, _, active := c.MetricsTarget(); active {
		t.Fatal("active before activation")
	}
	if err := c.Activate(context.Background(), cfgOf("otel", map[string]string{"pool": "false"})); err != nil {
		t.Fatal(err)
	}
	if url, token, active := c.MetricsTarget(); !active || url != "http://collector.example" || token != "test-token" {
		t.Fatalf("target %q %q %v", url, token, active)
	}
	t.Setenv("BASHY_OTEL_URL", "http://override.example")
	if url, token, active := c.MetricsTarget(); !active || url != "http://override.example" || token != "" {
		t.Fatalf("override target %q %q %v", url, token, active)
	}
	if err := c.Deactivate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, active := c.MetricsTarget(); active {
		t.Fatal("active after deactivation")
	}
}

func TestControlPlaneTelemetrySkipsNonHost(t *testing.T) {
	// With a live context, an accidentally started reconciler would block Wait.
	g, ctx := errgroup.WithContext(context.Background())
	startControlPlaneTelemetry(ctx, g, &conf.FileConfig{}, nil)
	if err := g.Wait(); err != nil {
		t.Fatal(err)
	}
}
