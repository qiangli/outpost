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
