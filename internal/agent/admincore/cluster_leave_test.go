package admincore

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// leaveTestServer builds an admincore.Server backed by a tempfile config and a
// fake ClusterRuntimeDown so a leave's runtime teardown is observable without a
// real podman. The returned pointers report whether the runtime dep ran, with
// what purge flag on its most recent invocation, and how many times total.
func leaveTestServer(t *testing.T) (s *Server, called *bool, purge *bool, calls *int) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("XDG_CACHE_HOME", tmp)
	t.Setenv("HOME", tmp)
	called = new(bool)
	purge = new(bool)
	calls = new(int)
	srv, err := New(Deps{
		ConfigPath: filepath.Join(tmp, "agent.json"),
		ClusterRuntimeDown: func(_ context.Context, p bool) error {
			*called = true
			*purge = p
			*calls++
			return nil
		},
	})
	if err != nil {
		t.Fatalf("admincore.New: %v", err)
	}
	return srv, called, purge, calls
}

// A peer-joined worker leaving must: stop the runtime WITHOUT purging its
// overlay identity (no headscale/overlay deregistration ever happened on the
// peer plane, so purging here would desync from a registration that still
// exists there), clear ONLY the peer membership fields, disable cluster mode,
// and leave cloudbox pairing + unrelated app/shell/LLM settings untouched.
// Result.Peer must be true so the CLI knows to skip the cloudbox reclaim.
func TestLeaveCluster_PeerWorker_ClearsPeerMembershipOnly(t *testing.T) {
	s, called, purge, _ := leaveTestServer(t)

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
	if !*called {
		t.Error("runtime teardown not called for an active peer leave")
	}
	if *purge {
		t.Error("purge=true for a peer leave; want false — peer leave preserves the overlay identity since no headscale/overlay deregistration occurred")
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

// Leaving is idempotent: a second leave is a no-op (no restart, no
// re-invocation of the runtime teardown dep).
//
// Note what this does NOT cover: bare JoinCluster is not a valid recovery
// path for a peer-joined worker. LeaveCluster clears join_endpoint,
// join_token, stcp_secret, and node_token, and JoinCluster only flips
// cluster.enabled back on — it does not restore any of the four peer
// credentials. A worker that calls JoinCluster after leaving a peer plane
// comes back with Runtimes.Agent still selected but JoinsPeerPlane() false,
// so it silently falls back to the cloudbox-hosted plane instead of
// rejoining the peer. See TestLeaveCluster_PeerWorker_RejoinNeedsFullCreds
// for the actual peer-recovery path (JoinPeerPlane with all four fields).
func TestLeaveCluster_Idempotent(t *testing.T) {
	s, _, _, calls := leaveTestServer(t)
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
	if *calls != 1 {
		t.Fatalf("runtime teardown called %d times on the first (active) leave, want 1", *calls)
	}
	// Second leave: already off (not active), so the runtime teardown dep must
	// NOT be invoked again — the call count must stay unchanged — and no
	// restart is requested.
	again, err := s.LeaveCluster(context.Background())
	if err != nil {
		t.Fatalf("second LeaveCluster: %v", err)
	}
	if *calls != 1 {
		t.Errorf("runtime teardown invoked again on an already-left node: calls=%d, want unchanged at 1", *calls)
	}
	if again.RestartPending {
		t.Error("a no-op leave requested a restart")
	}
	if again.Peer {
		t.Error("Result.Peer true after peer membership was already cleared")
	}
}

// A peer-joined worker cannot recover with a bare JoinCluster — LeaveCluster
// cleared join_endpoint, join_token, stcp_secret, and node_token, and only a
// FULL JoinPeerPlane call (endpoint + all three credentials) restores peer
// membership. This exercises that actual recovery path and asserts every
// credential comes back, the runtime selection LeaveCluster preserved is
// still intact, and cluster mode is enabled.
func TestLeaveCluster_PeerWorker_RejoinNeedsFullCreds(t *testing.T) {
	s, _, _, _ := leaveTestServer(t)
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
		t.Fatalf("LeaveCluster: %v", err)
	}
	fc, _ := conf.LoadFile(s.deps.ConfigPath)
	if fc.Cluster.JoinsPeerPlane() {
		t.Fatalf("still joins a peer plane right after leave: %+v", fc.Cluster)
	}

	endpoint, token, secret, node := "10.0.0.5:7000", "t2", "s2", "n2"
	rj, err := s.JoinPeerPlane(PeerPlaneParams{
		Endpoint:   &endpoint,
		Token:      &token,
		STCPSecret: &secret,
		NodeToken:  &node,
	})
	if err != nil {
		t.Fatalf("JoinPeerPlane: %v", err)
	}
	if !rj.Joined {
		t.Errorf("JoinPeerPlane result reports not joined: %+v", rj)
	}
	if !rj.HasToken || !rj.HasSTCPSecret || !rj.HasNodeToken {
		t.Errorf("JoinPeerPlane result missing a restored credential: %+v", rj)
	}
	if !rj.ClusterEnabled {
		t.Error("JoinPeerPlane did not enable cluster mode")
	}
	if !rj.RestartPending {
		t.Error("rejoin of a named host did not request a restart")
	}

	fc, _ = conf.LoadFile(s.deps.ConfigPath)
	if !fc.Cluster.JoinsPeerPlane() {
		t.Errorf("rejoin did not restore peer-plane membership: %+v", fc.Cluster)
	}
	if fc.Cluster.JoinEndpoint != endpoint || fc.Cluster.JoinToken != token ||
		fc.Cluster.STCPSecret != secret || fc.Cluster.NodeToken != node {
		t.Errorf("rejoin did not restore all four peer credentials: %+v", fc.Cluster)
	}
	if !fc.ClusterOn() {
		t.Error("rejoin left cluster mode disabled")
	}
	// Runtime selection LeaveCluster preserved is still what a rejoin comes
	// back as.
	if !fc.Cluster.Runtimes.Agent {
		t.Errorf("rejoin lost the preserved runtime selection: %+v", fc.Cluster)
	}
}

// A cloud-managed node (no peer endpoint) leaving reports Peer=false — the CLI
// keeps its cloudbox-reclaim path — and clears the cloud-issued creds. It also
// purges its local overlay identity (purge=true): cloudbox has already
// deregistered it from Headscale, so keeping the stale machine key would
// strand a rejoin.
func TestLeaveCluster_CloudNode_ReportsNotPeer(t *testing.T) {
	s, called, purge, _ := leaveTestServer(t)
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
	if !*called {
		t.Error("runtime teardown not called for an active cloud leave")
	}
	if !*purge {
		t.Error("purge=false for a cloud leave; want true — cloudbox already deregistered this node from Headscale")
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
	s, _, _, _ := leaveTestServer(t)
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
