package main

import (
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// THE REGRESSION THIS FILE EXISTS FOR.
//
// A host pointed at a peer-hosted control plane connected its tunnel and
// reached the right apiserver — and then failed to join, because the boot
// reattach had already replaced the PEER's node token with CLOUDBOX's. k3s
// rejected it with a CA-hash mismatch, which reads like a broken tunnel while
// the tunnel was in fact working. Cluster membership belongs to whichever
// plane the host joined; cloudbox's copy describes a different cluster.
func TestApplyCloudboxClusterMembership_OverwritesWhenJoiningCloudbox(t *testing.T) {
	fc := &conf.FileConfig{Cluster: &conf.ClusterConfig{
		NodeToken:  "K10old::node:old",
		STCPSecret: "old-secret",
	}}
	refreshed := &conf.FileConfig{Cluster: &conf.ClusterConfig{
		NodeToken:  "K10cloudbox::node:new",
		STCPSecret: "cloudbox-secret",
	}}
	if !applyCloudboxClusterMembership(fc, refreshed) {
		t.Fatal("expected a change to be reported")
	}
	if fc.Cluster.NodeToken != "K10cloudbox::node:new" {
		t.Errorf("node token = %q, want cloudbox's", fc.Cluster.NodeToken)
	}
	if fc.Cluster.STCPSecret != "cloudbox-secret" {
		t.Errorf("stcp secret = %q, want cloudbox's", fc.Cluster.STCPSecret)
	}
}

// Idempotence: an unchanged refresh must not report a change, or every boot
// rewrites the config file for nothing. The steady state includes the
// mirrored CloudSTCPSecret — its first capture IS a change (asserted
// separately below), after which refreshes go quiet again.
func TestApplyCloudboxClusterMembership_NoChangeWhenIdentical(t *testing.T) {
	same := func() *conf.FileConfig {
		return &conf.FileConfig{Cluster: &conf.ClusterConfig{
			NodeToken: "K10same::node:same", STCPSecret: "same", K8sAPIPort: 6443,
			CloudSTCPSecret: "same",
		}}
	}
	if applyCloudboxClusterMembership(same(), same()) {
		t.Error("reported a change for an identical refresh")
	}
}

// The relay secret's capture semantics on a plain cloudbox member: the
// dedicated field mirrors STCPSecret, reports a change exactly once, and
// goes quiet on the next identical refresh.
func TestApplyCloudboxClusterMembership_CapturesCloudSTCPSecretOnce(t *testing.T) {
	fc := &conf.FileConfig{Cluster: &conf.ClusterConfig{}}
	refreshed := &conf.FileConfig{Cluster: &conf.ClusterConfig{STCPSecret: "cloud-secret"}}
	if !applyCloudboxClusterMembership(fc, refreshed) {
		t.Fatal("first capture must report a change so it persists")
	}
	if fc.Cluster.CloudSTCPSecret != "cloud-secret" {
		t.Fatalf("cloud_stcp_secret = %q, want mirrored", fc.Cluster.CloudSTCPSecret)
	}
	if applyCloudboxClusterMembership(fc, refreshed) {
		t.Error("second identical refresh reported a change")
	}
}

// The guard itself: a host with a join endpoint must keep the peer's
// credentials. This is the assertion that would have caught the live failure.
func TestJoinsPeerPlane_GatesMembershipOverwrite(t *testing.T) {
	peer := &conf.ClusterConfig{
		JoinEndpoint: "10.0.0.5:7000",
		NodeToken:    "K10peer::node:peer",
	}
	if !peer.JoinsPeerPlane() {
		t.Fatal("a configured join endpoint must report as a peer plane")
	}
	cloud := &conf.ClusterConfig{NodeToken: "K10cloud::node:cloud"}
	if cloud.JoinsPeerPlane() {
		t.Error("no join endpoint must mean the cloudbox plane")
	}
	// Nil must be safe — callers reach this through fc.Cluster, nil on an
	// unconfigured host.
	if (*conf.ClusterConfig)(nil).JoinsPeerPlane() {
		t.Error("nil config reported as joining a peer plane")
	}
}

// B6 — THE HALF THE NODE-TOKEN GUARD LEFT BEHIND. Skipping cloudbox's refresh
// keeps a peer member's own credentials, but a host that MOVED from the
// cloudbox plane to a peer plane still carries the cloud plane's overlay trio
// on disk, and the runtime joins an overlay purely on overlay_login_server
// being non-empty (runtime.Up). Verified on hardware: the k3s agent joined the
// peer plane while tailscaled joined the CLOUD overlay. The reattach must
// therefore clear the trio, not merely ignore cloudbox's refreshed copy.
func TestReconcileClusterMembershipOnReattach(t *testing.T) {
	cloudRefresh := func() *conf.FileConfig {
		return &conf.FileConfig{Cluster: &conf.ClusterConfig{
			NodeToken:          "K10cloud::node:new",
			STCPSecret:         "cloud-secret",
			K8sAPIPort:         16443,
			OverlayLoginServer: "https://ai.example.io/overlay",
			OverlayAuthKey:     "ts-authkey-cloud",
			OverlayPodCIDR:     "10.42.7.0/24",
		}}
	}
	tests := []struct {
		name        string
		cluster     *conf.ClusterConfig
		wantChanged bool
		check       func(t *testing.T, cc *conf.ClusterConfig)
	}{
		{
			// Unchanged behavior: a plain cloudbox member takes the whole
			// refresh, overlay trio included.
			name: "cloudbox member takes the refresh",
			cluster: &conf.ClusterConfig{
				NodeToken:  "K10old::node:old",
				STCPSecret: "old-secret",
				// F16: host-authoritative, never sent by cloudbox — the boot
				// reattach must leave it exactly as persisted.
				ControlPlaneKubeconfig: "/persisted/kubeconfig.yaml",
			},
			wantChanged: true,
			check: func(t *testing.T, cc *conf.ClusterConfig) {
				if cc.NodeToken != "K10cloud::node:new" || cc.STCPSecret != "cloud-secret" {
					t.Errorf("membership not refreshed: %+v", cc)
				}
				if cc.OverlayLoginServer != "https://ai.example.io/overlay" ||
					cc.OverlayAuthKey != "ts-authkey-cloud" ||
					cc.OverlayPodCIDR != "10.42.7.0/24" {
					t.Errorf("overlay trio not refreshed: %+v", cc)
				}
				if cc.ControlPlaneKubeconfig != "/persisted/kubeconfig.yaml" {
					t.Errorf("control_plane_kubeconfig clobbered by reattach: %q", cc.ControlPlaneKubeconfig)
				}
			},
		},
		{
			// The B6 case: peer credentials kept, stale cloud overlay dropped.
			name: "peer member keeps its credentials and drops the cloud overlay trio",
			cluster: &conf.ClusterConfig{
				JoinEndpoint:       "10.0.0.5:7000",
				JoinToken:          "peer-tunnel-token",
				NodeToken:          "K10peer::node:peer",
				STCPSecret:         "peer-secret",
				OverlayLoginServer: "https://ai.example.io/overlay", // stale cloud leftovers
				OverlayAuthKey:     "ts-authkey-cloud",
				OverlayPodCIDR:     "10.42.7.0/24",
			},
			wantChanged: true,
			check: func(t *testing.T, cc *conf.ClusterConfig) {
				if cc.NodeToken != "K10peer::node:peer" || cc.STCPSecret != "peer-secret" {
					t.Errorf("peer credentials clobbered: %+v", cc)
				}
				if cc.OverlayLoginServer != "" || cc.OverlayAuthKey != "" || cc.OverlayPodCIDR != "" {
					t.Errorf("cloud overlay trio survived: %+v", cc)
				}
			},
		},
		{
			// Idempotence: a peer member with nothing to drop must not report
			// a change, or every boot rewrites agent.json for nothing. The
			// steady state includes the already-captured relay secret.
			name: "peer member with no stale overlay is a no-op",
			cluster: &conf.ClusterConfig{
				JoinEndpoint:    "10.0.0.5:7000",
				NodeToken:       "K10peer::node:peer",
				STCPSecret:      "peer-secret",
				CloudSTCPSecret: "cloud-secret",
			},
			wantChanged: false,
			check: func(t *testing.T, cc *conf.ClusterConfig) {
				if cc.NodeToken != "K10peer::node:peer" || cc.STCPSecret != "peer-secret" {
					t.Errorf("no-op mutated peer credentials: %+v", cc)
				}
			},
		},
		{
			// THE RELAY CAPTURE — the one cloudbox-plane value a peer member
			// takes from a reattach. cloudbox's STCP secret lands in its
			// DEDICATED field (the runtime's overlay-control relay needs it to
			// reach Headscale); the peer's own STCPSecret must not be touched.
			name: "peer member captures cloudbox's stcp secret into the relay field",
			cluster: &conf.ClusterConfig{
				JoinEndpoint: "10.0.0.5:7000",
				NodeToken:    "K10peer::node:peer",
				STCPSecret:   "peer-secret",
			},
			wantChanged: true,
			check: func(t *testing.T, cc *conf.ClusterConfig) {
				if cc.CloudSTCPSecret != "cloud-secret" {
					t.Errorf("cloud_stcp_secret = %q, want cloudbox's", cc.CloudSTCPSecret)
				}
				if cc.STCPSecret != "peer-secret" {
					t.Errorf("peer stcp secret clobbered: %q", cc.STCPSecret)
				}
				if cc.NodeToken != "K10peer::node:peer" {
					t.Errorf("peer node token clobbered: %q", cc.NodeToken)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fc := &conf.FileConfig{Cluster: tt.cluster}
			if got := reconcileClusterMembershipOnReattach(fc, cloudRefresh()); got != tt.wantChanged {
				t.Errorf("changed = %v, want %v", got, tt.wantChanged)
			}
			tt.check(t, fc.Cluster)
		})
	}
}

// JoinTarget is what decouples "which cluster" from "which cloudbox". The
// default must stay byte-identical to the pre-existing behaviour, or every
// already-deployed host changes plane on upgrade.
func TestJoinTarget_DefaultsToCloudboxPairing(t *testing.T) {
	fc := &conf.FileConfig{
		ServerAddr: "ai.example.io", ServerPort: 443, Token: "matrix-token",
		Cluster: &conf.ClusterConfig{},
	}
	host, port, token, user := fc.JoinTarget()
	if host != "ai.example.io" || port != 443 || token != "matrix-token" || user != "cloudbox" {
		t.Fatalf("got %s:%d token=%q user=%q, want the cloudbox pairing", host, port, token, user)
	}
}

func TestJoinTarget_UsesPeerPlaneWhenConfigured(t *testing.T) {
	fc := &conf.FileConfig{
		ServerAddr: "ai.example.io", ServerPort: 443, Token: "matrix-token",
		Cluster: &conf.ClusterConfig{
			JoinEndpoint: "10.0.0.5:7000", JoinToken: "peer-token",
		},
	}
	host, port, token, user := fc.JoinTarget()
	if host != "10.0.0.5" || port != 7000 {
		t.Errorf("got %s:%d, want the peer endpoint", host, port)
	}
	if token != "peer-token" {
		t.Errorf("token = %q, want the peer's", token)
	}
	// frp scopes STCP visibility BY USER: naming cloudbox here would be
	// refused by the peer's server rather than misrouted.
	if user != conf.ControlPlanePublisherUser {
		t.Errorf("serverUser = %q, want %q", user, conf.ControlPlanePublisherUser)
	}
}

// A bare host with no port must fall back to the default rather than yielding
// port 0, which would dial nothing.
func TestJoinTarget_BareHostGetsDefaultPort(t *testing.T) {
	fc := &conf.FileConfig{Cluster: &conf.ClusterConfig{JoinEndpoint: "10.0.0.5"}}
	host, port, _, _ := fc.JoinTarget()
	if host != "10.0.0.5" || port != conf.DefaultTunnelBindPort {
		t.Errorf("got %s:%d, want 10.0.0.5:%d", host, port, conf.DefaultTunnelBindPort)
	}
}

// THE TWO ROWS THAT WERE LIVE-WRONG. Both were produced by clusterReport
// reading cc.APIURL (cloudbox-issued) and ControlPlaneOn() (a fact about the
// host) after hosting and joining became independent decisions.
func TestClusterReport_JoinedPeerPlaneIsNotReportedAsCloudbox(t *testing.T) {
	const h = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	fc := &conf.FileConfig{
		AgentName: "worker",
		Cluster: &conf.ClusterConfig{
			Enabled:      boolPtr(true),
			Runtimes:     conf.ClusterRuntimes{Agent: true},
			APIURL:       "https://ai.example.io/api/cluster/agent", // stale cloudbox value
			JoinEndpoint: "10.0.0.5:7000",
			NodeToken:    "K10" + h + "::node:x",
		},
	}
	got := clusterReport(fc, "https://ai.example.io")
	if got == nil {
		t.Fatal("no report")
	}
	if got.Placement != "peer" {
		t.Errorf("placement = %q, want peer — the host moved to a peer plane", got.Placement)
	}
	if got.ControlPlane {
		t.Error("a worker reported itself as the cluster's control plane")
	}
	// Identity must be the peer cluster's, not cloudbox's URL.
	if id := got.ClusterID(); id != "k8s-"+h[:12] {
		t.Errorf("cluster id = %q, want the peer cluster's", id)
	}
}

// A host that HOSTS a plane while JOINING cloudbox's must not claim to be
// cloudbox's control plane — that would name the wrong host as the cluster's
// owner on the inventory page.
func TestClusterReport_HostingAPlaneDoesNotClaimTheJoinedOne(t *testing.T) {
	fc := &conf.FileConfig{
		AgentName: "dual",
		Cluster: &conf.ClusterConfig{
			Enabled:      boolPtr(true),
			Runtimes:     conf.ClusterRuntimes{Agent: true},
			APIURL:       "https://ai.example.io/api/cluster/agent",
			ControlPlane: boolPtr(true), // hosts a plane for OTHER machines
		},
	}
	got := clusterReport(fc, "https://ai.example.io")
	if got == nil {
		t.Fatal("no report")
	}
	if got.ControlPlane {
		t.Error("claimed to be the control plane of the cluster it merely joined")
	}
	if got.Placement != "cloudbox" {
		t.Errorf("placement = %q, want cloudbox — that is the cluster it is a node of", got.Placement)
	}
	// The hosting fact is still reported, just not conflated with the row.
	if !got.HostsControlPlane || got.ControlPlaneEndpoint == "" {
		t.Errorf("hosting fact lost: hosts=%v endpoint=%q", got.HostsControlPlane, got.ControlPlaneEndpoint)
	}
}

// The genuinely self-hosted case: hosts the plane AND is a node of it.
func TestClusterReport_SelfHostedClaimsItsOwnCluster(t *testing.T) {
	fc := &conf.FileConfig{
		AgentName: "solo",
		Cluster: &conf.ClusterConfig{
			Enabled:      boolPtr(true),
			Runtimes:     conf.ClusterRuntimes{Agent: true},
			APIURL:       "https://127.0.0.1:6443",
			ControlPlane: boolPtr(true),
		},
	}
	got := clusterReport(fc, "https://ai.example.io")
	if got == nil || !got.ControlPlane || got.Placement != "self" {
		t.Fatalf("got %+v, want a self-hosted control plane", got)
	}
}
