package admincore

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// leaveTestServer builds an admincore.Server backed by a tempfile config and a
// fake ClusterRuntimeDown so a leave's runtime teardown is observable without a
// real podman. The returned pointers report whether the runtime dep ran and
// with what purge flag.
func leaveTestServer(t *testing.T) (s *Server, called *bool, purge *bool) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("XDG_CACHE_HOME", tmp)
	t.Setenv("HOME", tmp)
	called = new(bool)
	purge = new(bool)
	srv, err := New(Deps{
		ConfigPath: filepath.Join(tmp, "agent.json"),
		ClusterRuntimeDown: func(_ context.Context, p bool) error {
			*called = true
			*purge = p
			return nil
		},
	})
	if err != nil {
		t.Fatalf("admincore.New: %v", err)
	}
	return srv, called, purge
}

// A peer-joined worker leaving must: stop the runtime (purge), clear ONLY the
// peer membership fields, disable cluster mode, and leave cloudbox pairing +
// unrelated app/shell/LLM settings untouched. Result.Peer must be true so the
// CLI knows to skip the cloudbox reclaim.
func TestLeaveCluster_PeerWorker_ClearsPeerMembershipOnly(t *testing.T) {
	s, called, purge := leaveTestServer(t)

	// A paired host that also joined a peer plane, plus unrelated settings we
	// must not disturb.
	if err := conf.SaveFile(s.deps.ConfigPath, &conf.FileConfig{
		AgentName:   "worker-1",
		AccessToken: "CLOUDBOX-PAIRING-TOKEN",
		LocalAddr:   "127.0.0.1:9999", // an unrelated app/networking setting
		Cluster: &conf.ClusterConfig{
			JoinEndpoint: "10.0.0.5:7000",
			JoinToken:    "tunnel-token",
			STCPSecret:   "stcp-secret",
			NodeToken:    "K10abc::node:xyz",
			K8sAPIPort:   6443,
			Runtimes:     conf.ClusterRuntimes{Agent: true},
		},
	}); err != nil {
		t.Fatal(err)
	}
	// Cluster mode on.
	enabled := true
	fc0, _ := conf.LoadFile(s.deps.ConfigPath)
	fc0.Cluster.Enabled = &enabled
	if err := conf.SaveFile(s.deps.ConfigPath, fc0); err != nil {
		t.Fatal(err)
	}

	out, err := s.LeaveCluster(context.Background())
	if err != nil {
		t.Fatalf("LeaveCluster: %v", err)
	}
	if !out.Peer {
		t.Errorf("Result.Peer = false for a peer-joined worker: %+v", out)
	}
	if !out.RestartPending {
		t.Errorf("RestartPending = false for a joined+named host: %+v", out)
	}
	if !*called || !*purge {
		t.Errorf("runtime teardown: called=%v purge=%v, want both true", *called, *purge)
	}

	fc, err := conf.LoadFile(s.deps.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	// Peer membership cleared, cluster disabled.
	if fc.Cluster.JoinsPeerPlane() {
		t.Errorf("still joins a peer plane after leave: %+v", fc.Cluster)
	}
	if fc.Cluster.JoinEndpoint != "" || fc.Cluster.JoinToken != "" ||
		fc.Cluster.NodeToken != "" || fc.Cluster.STCPSecret != "" {
		t.Errorf("peer membership not cleared: %+v", fc.Cluster)
	}
	if fc.ClusterOn() {
		t.Error("cluster mode still on after leave")
	}
	// Runtime SELECTION preserved so a rejoin returns as the same kind of node.
	if !fc.Cluster.Runtimes.Agent {
		t.Error("runtime selection wiped by leave")
	}
	// Cloudbox pairing + unrelated settings preserved: leaving the cluster is
	// not logging out of the portal.
	if fc.AccessToken != "CLOUDBOX-PAIRING-TOKEN" {
		t.Errorf("leave unpaired the host: access_token = %q", fc.AccessToken)
	}
	if fc.AgentName != "worker-1" {
		t.Errorf("leave changed agent_name: %q", fc.AgentName)
	}
	if fc.LocalAddr != "127.0.0.1:9999" {
		t.Errorf("leave disturbed an unrelated setting: local_addr = %q", fc.LocalAddr)
	}
}

// Leaving is idempotent and recoverable: a second leave is a no-op (no restart),
// and a rejoin re-enables cluster mode retaining the preserved runtime set.
func TestLeaveCluster_IdempotentAndRejoinable(t *testing.T) {
	s, _, _ := leaveTestServer(t)
	enabled := true
	if err := conf.SaveFile(s.deps.ConfigPath, &conf.FileConfig{
		AgentName: "worker-1",
		Cluster: &conf.ClusterConfig{
			Enabled:      &enabled,
			JoinEndpoint: "10.0.0.5:7000",
			JoinToken:    "t",
			NodeToken:    "n",
			STCPSecret:   "s",
			Runtimes:     conf.ClusterRuntimes{Agent: true},
		},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.LeaveCluster(context.Background()); err != nil {
		t.Fatalf("first LeaveCluster: %v", err)
	}
	// Second leave: already off, so no restart requested.
	again, err := s.LeaveCluster(context.Background())
	if err != nil {
		t.Fatalf("second LeaveCluster: %v", err)
	}
	if again.RestartPending {
		t.Error("a no-op leave requested a restart")
	}
	if again.Peer {
		t.Error("Result.Peer true after peer membership was already cleared")
	}

	// Rejoin re-enables, retaining the runtime LeaveCluster preserved.
	rj, err := s.JoinCluster()
	if err != nil {
		t.Fatalf("JoinCluster: %v", err)
	}
	if !rj.RestartPending {
		t.Error("rejoin of a named host did not request a restart")
	}
	fc, _ := conf.LoadFile(s.deps.ConfigPath)
	if !fc.ClusterOn() || !fc.Cluster.Runtimes.Agent {
		t.Errorf("rejoin did not restore an enabled agent node: %+v", fc.Cluster)
	}
}

// A cloud-managed node (no peer endpoint) leaving reports Peer=false — the CLI
// keeps its cloudbox-reclaim path — and clears the cloud-issued creds.
func TestLeaveCluster_CloudNode_ReportsNotPeer(t *testing.T) {
	s, called, purge := leaveTestServer(t)
	enabled := true
	if err := conf.SaveFile(s.deps.ConfigPath, &conf.FileConfig{
		AgentName:   "cloud-node",
		AccessToken: "tok",
		Cluster: &conf.ClusterConfig{
			Enabled:            &enabled,
			APIURL:             "https://ai.dhnt.io/api/cluster/agent",
			Token:              "sa-token",
			CA:                 []byte("PEM"),
			NodeToken:          "K10cloud::node:abc",
			OverlayLoginServer: "https://ai.dhnt.io/overlay/headscale",
			OverlayAuthKey:     "ts-key",
			OverlayPodCIDR:     "10.42.3.0/24",
			Runtimes:           conf.ClusterRuntimes{Agent: true},
		},
	}); err != nil {
		t.Fatal(err)
	}
	out, err := s.LeaveCluster(context.Background())
	if err != nil {
		t.Fatalf("LeaveCluster: %v", err)
	}
	if out.Peer {
		t.Errorf("cloud node reported Peer=true: %+v", out)
	}
	if !*called || !*purge {
		t.Errorf("runtime teardown: called=%v purge=%v", *called, *purge)
	}
	fc, _ := conf.LoadFile(s.deps.ConfigPath)
	if fc.Cluster.APIURL != "" || fc.Cluster.Token != "" || fc.Cluster.CA != nil ||
		fc.Cluster.OverlayLoginServer != "" || fc.Cluster.OverlayAuthKey != "" ||
		fc.Cluster.OverlayPodCIDR != "" || fc.Cluster.NodeToken != "" {
		t.Errorf("cloud-issued membership not cleared: %+v", fc.Cluster)
	}
}

// Leaving the cluster this host JOINS must not corrupt the plane this host
// HOSTS: a control-plane host keeps its tunnel_token, stcp_secret, control_plane
// flag, and bind config so every worker joined to it still authenticates.
func TestLeaveCluster_PreservesHostedControlPlane(t *testing.T) {
	s, _, _ := leaveTestServer(t)
	enabled, hosting := true, true
	if err := conf.SaveFile(s.deps.ConfigPath, &conf.FileConfig{
		AgentName: "plane-host",
		Cluster: &conf.ClusterConfig{
			Enabled: &enabled,
			// This host BOTH hosts a plane and joined a peer plane itself.
			ControlPlane:        &hosting,
			TunnelToken:         "HOSTING-TUNNEL-TOKEN",
			STCPSecret:          "HOSTING-STCP-SECRET",
			TunnelBindAddr:      "0.0.0.0",
			TunnelBindPort:      7000,
			ControlPlaneAPIAddr: "127.0.0.1:16443",
			JoinEndpoint:        "10.0.0.9:7000",
			JoinToken:           "peer-tunnel-token",
			NodeToken:           "K10peer::node:abc",
			Runtimes:            conf.ClusterRuntimes{Agent: true},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LeaveCluster(context.Background()); err != nil {
		t.Fatalf("LeaveCluster: %v", err)
	}
	fc, _ := conf.LoadFile(s.deps.ConfigPath)
	cc := fc.Cluster
	// Hosting block intact.
	if !cc.ControlPlaneOn() {
		t.Error("leave cleared the control_plane hosting flag")
	}
	if cc.TunnelToken != "HOSTING-TUNNEL-TOKEN" {
		t.Errorf("leave cleared the hosting tunnel_token: %q", cc.TunnelToken)
	}
	if cc.STCPSecret != "HOSTING-STCP-SECRET" {
		t.Errorf("leave cleared the hosting stcp_secret: %q", cc.STCPSecret)
	}
	if cc.TunnelBindAddr != "0.0.0.0" || cc.TunnelBindPort != 7000 ||
		cc.ControlPlaneAPIAddr != "127.0.0.1:16443" {
		t.Errorf("leave disturbed the hosting bind config: %+v", cc)
	}
	// Peer membership this host JOINED is still cleared.
	if cc.JoinEndpoint != "" || cc.JoinToken != "" || cc.NodeToken != "" {
		t.Errorf("peer membership not cleared: %+v", cc)
	}
}
