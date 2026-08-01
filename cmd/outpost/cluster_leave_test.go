package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// writeLeaveConfig writes an agent.json at the XDG-resolved path for a leave
// test and returns nothing — the command reads it via conf.DefaultConfigPath.
func writeLeaveConfig(t *testing.T, fc *conf.FileConfig) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("XDG_CACHE_HOME", tmp)
	t.Setenv("HOME", tmp)
	dir := filepath.Join(tmp, "matrix")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := conf.SaveFile(filepath.Join(dir, "agent.json"), fc); err != nil {
		t.Fatal(err)
	}
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what it
// printed. The leave command prints its confirmation to fmt.Println (stdout).
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() { os.Stdout = orig }()
	fn()
	_ = w.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	return buf.String()
}

// Without --yes the leave command prints a plane-appropriate confirmation and
// returns BEFORE touching the daemon, so the branch is deterministically
// testable. A peer-joined worker must be told the node is not cloudbox's to
// reclaim and not the worker's to delete from the peer apiserver.
func TestClusterLeave_Confirmation_PeerWorker(t *testing.T) {
	writeLeaveConfig(t, &conf.FileConfig{
		AgentName:   "worker-1",
		AccessToken: "tok",
		Cluster: &conf.ClusterConfig{
			JoinEndpoint: "10.0.0.5:7000",
			JoinToken:    "t",
		},
	})
	cmd := clusterLeaveCmd()
	cmd.SetArgs([]string{})
	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("leave (no --yes): %v", err)
		}
	})
	if !strings.Contains(out, "PEER-hosted control plane") {
		t.Errorf("peer confirmation missing peer-plane note:\n%s", out)
	}
	if !strings.Contains(out, "does NOT delete this node from the peer apiserver") {
		t.Errorf("peer confirmation missing node-deletion boundary:\n%s", out)
	}
	if strings.Contains(out, "pod CIDR on cloudbox") {
		t.Errorf("peer confirmation wrongly claims a cloudbox reclaim:\n%s", out)
	}
	// No secret values must appear in operator-facing output.
	if strings.Contains(out, "10.0.0.5:7000") || strings.Contains(out, "\"t\"") {
		t.Errorf("confirmation leaked config values:\n%s", out)
	}
}

// A cloud-managed node keeps the cloudbox-reclaim wording.
func TestClusterLeave_Confirmation_CloudNode(t *testing.T) {
	writeLeaveConfig(t, &conf.FileConfig{
		AgentName:   "cloud-node",
		AccessToken: "tok",
		Cluster:     &conf.ClusterConfig{Runtimes: conf.ClusterRuntimes{Agent: true}},
	})
	cmd := clusterLeaveCmd()
	cmd.SetArgs([]string{})
	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("leave (no --yes): %v", err)
		}
	})
	if !strings.Contains(out, "pod CIDR on cloudbox") {
		t.Errorf("cloud confirmation missing cloudbox-reclaim wording:\n%s", out)
	}
	if strings.Contains(out, "PEER-hosted control plane") {
		t.Errorf("cloud node got the peer confirmation:\n%s", out)
	}
}

// fakeClusterLeaveDeps builds clusterLeaveDeps whose notifyLeave counts its
// own invocations, so the plane-dependent --yes decision (does cloudbox get
// notified at all) is assertable without a live cloudbox or a running outpost
// daemon (leaveLocal/restart are stubbed to no-ops).
func fakeClusterLeaveDeps(notifyCalls *int) clusterLeaveDeps {
	return clusterLeaveDeps{
		notifyLeave: func(_ context.Context, _, _, _ string) (string, error) {
			*notifyCalls++
			return "left", nil
		},
		leaveLocal: func(_ context.Context) (bool, error) { return false, nil },
		restart:    func(_ context.Context) error { return nil },
	}
}

// A peer-joined worker's --yes leave must invoke notifyLeave exactly ZERO
// times: cloudbox never issued the node, so there is nothing to reclaim, and
// the worker holds no admin credential to delete it from the peer apiserver.
// Confirmation-text-only coverage (TestClusterLeave_Confirmation_PeerWorker)
// only exercises the !yes early-return branch — it cannot see this decision.
func TestClusterLeave_Yes_PeerWorker_SkipsCloudboxNotify(t *testing.T) {
	fc := &conf.FileConfig{
		AgentName:   "worker-1",
		AccessToken: "tok",
		ServerAddr:  "ai.dhnt.io",
		Cluster: &conf.ClusterConfig{
			JoinEndpoint: "10.0.0.5:7000",
			JoinToken:    "t",
		},
	}
	var notifyCalls int
	var buf bytes.Buffer
	if err := runClusterLeave(context.Background(), fc, true, &buf, fakeClusterLeaveDeps(&notifyCalls)); err != nil {
		t.Fatalf("runClusterLeave: %v", err)
	}
	if notifyCalls != 0 {
		t.Errorf("notifyLeave invoked %d times for a peer-joined worker, want 0", notifyCalls)
	}
}

// A cloud-managed node's --yes leave must invoke notifyLeave exactly ONCE —
// cloudbox owns that node's k8s Node, overlay registration, and pod-CIDR
// reservation, and leave's whole job is to tell it to reclaim them.
func TestClusterLeave_Yes_CloudNode_NotifiesCloudboxOnce(t *testing.T) {
	fc := &conf.FileConfig{
		AgentName:   "cloud-node",
		AccessToken: "tok",
		ServerAddr:  "ai.dhnt.io",
		Cluster:     &conf.ClusterConfig{Runtimes: conf.ClusterRuntimes{Agent: true}},
	}
	var notifyCalls int
	var buf bytes.Buffer
	if err := runClusterLeave(context.Background(), fc, true, &buf, fakeClusterLeaveDeps(&notifyCalls)); err != nil {
		t.Fatalf("runClusterLeave: %v", err)
	}
	if notifyCalls != 1 {
		t.Errorf("notifyLeave invoked %d times for a cloud-managed node, want 1", notifyCalls)
	}
}
