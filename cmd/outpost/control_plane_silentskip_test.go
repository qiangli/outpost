package main

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sync/errgroup"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// captureSlog swaps the default logger for the duration of a test and returns a
// buffer holding everything it emitted.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// A2 — the decline used to be SILENT. A host meant to host the control plane
// (cluster.control_plane=true) whose flag got dropped on register/recover would
// produce NO log line, leaving the operator nothing to diagnose while workers
// failed to join. The fix: a host carrying control-plane credentials but with
// the flag off is the signature of a dropped flag, and it must say so at Warn.
func TestStartControlPlaneTunnel_WarnsOnResidualCredentialsWithFlagOff(t *testing.T) {
	buf := captureSlog(t)
	isolateFromPodman(t)
	dir := t.TempDir()

	fc := &conf.FileConfig{
		AgentName: "cp-host",
		Cluster: &conf.ClusterConfig{
			// control_plane flag absent (nil == off), yet this host still holds
			// a control-plane tunnel token from when it WAS the plane.
			TunnelToken: strings.Repeat("a", 64),
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g, gctx := errgroup.WithContext(ctx)
	startControlPlaneTunnel(gctx, g, fc, filepath.Join(dir, "agent.json"))
	cancel()
	_ = g.Wait()

	if out := buf.String(); !strings.Contains(out, "control_plane flag is off") {
		t.Errorf("declining with residual credentials must log why at Warn; got:\n%s", out)
	}
}

// The ordinary non-control-plane host must still say why it is not hosting —
// no branch of this function may decline silently — but at Info, not Warn,
// since a host that was never a control plane is not a problem.
func TestStartControlPlaneTunnel_LogsPlainDeclineWhenNotControlPlane(t *testing.T) {
	buf := captureSlog(t)
	isolateFromPodman(t)
	dir := t.TempDir()

	fc := &conf.FileConfig{AgentName: "worker", Cluster: &conf.ClusterConfig{}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g, gctx := errgroup.WithContext(ctx)
	startControlPlaneTunnel(gctx, g, fc, filepath.Join(dir, "agent.json"))
	cancel()
	_ = g.Wait()

	out := buf.String()
	if !strings.Contains(out, "not hosting") {
		t.Errorf("a declining branch logged nothing; got:\n%s", out)
	}
	if strings.Contains(out, "control-plane credentials") {
		t.Errorf("a plain worker was misreported as a dropped-flag control plane; got:\n%s", out)
	}
}
