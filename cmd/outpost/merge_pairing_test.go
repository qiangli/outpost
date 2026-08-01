package main

import (
	"path/filepath"
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// F16 — THE CLOBBER SITE FOR control_plane_kubeconfig.
//
// mergePairing used to rebuild the Cluster block from cloudbox's copy and
// hand-list the surviving host fields (Enabled / Runtimes / APIURL / Token /
// CA / NodeName). Every host-authoritative field NOT on that list was silently
// dropped on `outpost register` / `recover` — including
// cluster.control_plane_kubeconfig, which rememberControlPlaneKubeconfig then
// re-derived on the next control-plane boot, making the loss invisible except
// to status surfaces on every host that is not a control plane. The merge is
// now inverted: start from the on-disk block and overlay only the fields
// cloudbox owns, so host fields survive by default.

func writeConfig(t *testing.T, fc *conf.FileConfig) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.json")
	if err := conf.SaveFile(path, fc); err != nil {
		t.Fatal(err)
	}
	return path
}

func exchangedWithCluster() *conf.FileConfig {
	return &conf.FileConfig{
		AgentName:   "host1",
		ServerAddr:  "ai.example.io",
		ServerPort:  443,
		Protocol:    "wss",
		Token:       "matrix-token",
		AccessToken: "access-token",
		Cluster: &conf.ClusterConfig{
			NodeToken:          "K10cloud::node:new",
			STCPSecret:         "cloud-secret",
			K8sAPIPort:         16443,
			OverlayLoginServer: "https://ai.example.io/overlay",
			OverlayAuthKey:     "ts-authkey-cloud",
			OverlayPodCIDR:     "10.42.7.0/24",
			MetricsRemoteURL:   "https://ai.example.io/otel/metrics",
		},
	}
}

// The F16 assertion: an explicitly persisted control_plane_kubeconfig — and
// the rest of the host-authoritative cluster fields — survive a re-pair.
func TestMergePairing_ControlPlaneHostKeepsItsClusterFields(t *testing.T) {
	on := true
	path := writeConfig(t, &conf.FileConfig{
		AgentName: "cp-host",
		Cluster: &conf.ClusterConfig{
			ControlPlane:           &on,
			ControlPlaneKubeconfig: "/operator/chose/this.yaml",
			TunnelToken:            "minted-tunnel-token-0123456789abcdef",
			STCPSecret:             "minted-stcp-secret-0123456789abcdef",
		},
	})
	merged := mergePairing(path, exchangedWithCluster())
	cc := merged.Cluster
	if cc.ControlPlaneKubeconfig != "/operator/chose/this.yaml" {
		t.Errorf("control_plane_kubeconfig clobbered: %q", cc.ControlPlaneKubeconfig)
	}
	if !cc.ControlPlaneOn() {
		t.Error("control_plane flag dropped — this is what booted a control-plane host as a worker")
	}
	if cc.TunnelToken != "minted-tunnel-token-0123456789abcdef" {
		t.Errorf("tunnel token clobbered: %q", cc.TunnelToken)
	}
	// The hosted plane's publish secret must not be swapped for cloudbox's —
	// that would lock out every worker holding the minted value.
	if cc.STCPSecret != "minted-stcp-secret-0123456789abcdef" {
		t.Errorf("stcp secret clobbered: %q", cc.STCPSecret)
	}
	// Pairing fields still come from the exchange.
	if merged.AgentName != "host1" || merged.Token != "matrix-token" {
		t.Errorf("pairing fields not applied: %+v", merged)
	}
}

// A plain cloudbox member's behavior is unchanged: the membership refresh
// (node token, secret, ports, overlay trio, observability URLs) applies.
func TestMergePairing_CloudboxMemberTakesMembershipRefresh(t *testing.T) {
	path := writeConfig(t, &conf.FileConfig{
		AgentName: "worker",
		Cluster: &conf.ClusterConfig{
			NodeToken:  "K10old::node:old",
			STCPSecret: "old-secret",
		},
	})
	merged := mergePairing(path, exchangedWithCluster())
	cc := merged.Cluster
	if cc.NodeToken != "K10cloud::node:new" || cc.STCPSecret != "cloud-secret" {
		t.Errorf("membership not refreshed: %+v", cc)
	}
	if cc.OverlayLoginServer != "https://ai.example.io/overlay" {
		t.Errorf("overlay not refreshed: %+v", cc)
	}
	if cc.MetricsRemoteURL != "https://ai.example.io/otel/metrics" {
		t.Errorf("observability endpoint not refreshed: %q", cc.MetricsRemoteURL)
	}
}

// B6 at the pairing surface: a peer-plane member keeps its own credentials and
// sheds any cloud overlay leftovers, same invariant as the boot reattach.
func TestMergePairing_PeerMemberKeepsCredentialsDropsCloudOverlay(t *testing.T) {
	path := writeConfig(t, &conf.FileConfig{
		AgentName: "worker",
		Cluster: &conf.ClusterConfig{
			JoinEndpoint:       "10.0.0.5:7000",
			JoinToken:          "peer-tunnel-token",
			NodeToken:          "K10peer::node:peer",
			STCPSecret:         "peer-secret",
			OverlayLoginServer: "https://ai.example.io/overlay", // stale cloud leftovers
			OverlayAuthKey:     "ts-authkey-cloud",
			OverlayPodCIDR:     "10.42.7.0/24",
		},
	})
	merged := mergePairing(path, exchangedWithCluster())
	cc := merged.Cluster
	if cc.NodeToken != "K10peer::node:peer" || cc.STCPSecret != "peer-secret" {
		t.Errorf("peer credentials clobbered: %+v", cc)
	}
	if cc.JoinEndpoint != "10.0.0.5:7000" || cc.JoinToken != "peer-tunnel-token" {
		t.Errorf("join fields dropped: %+v", cc)
	}
	if cc.OverlayLoginServer != "" || cc.OverlayAuthKey != "" || cc.OverlayPodCIDR != "" {
		t.Errorf("cloud overlay trio survived the re-pair: %+v", cc)
	}
}

// A fresh host has nothing to preserve — cloudbox's block is taken wholesale.
func TestMergePairing_FreshHostTakesCloudboxClusterBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.json") // no file on disk
	merged := mergePairing(path, exchangedWithCluster())
	if merged.Cluster == nil || merged.Cluster.NodeToken != "K10cloud::node:new" {
		t.Errorf("fresh host did not take cloudbox's cluster block: %+v", merged.Cluster)
	}
}
