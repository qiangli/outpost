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
// rewrites the config file for nothing.
func TestApplyCloudboxClusterMembership_NoChangeWhenIdentical(t *testing.T) {
	same := func() *conf.FileConfig {
		return &conf.FileConfig{Cluster: &conf.ClusterConfig{
			NodeToken: "K10same::node:same", STCPSecret: "same", K8sAPIPort: 6443,
		}}
	}
	if applyCloudboxClusterMembership(same(), same()) {
		t.Error("reported a change for an identical refresh")
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
