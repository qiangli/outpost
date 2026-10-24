package admincore

import (
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// S498.4: a re-pair onto a different server orphans a plain server-plane
// member — the cached join is dropped AND cluster mode is disabled, so no
// boot re-fetches a fresh join from the new server (a third-party host
// re-paired to another owner's satellite silently joined its cluster).
// Runtime selection and node name survive for the explicit
// rejoin. Same-server re-pairs, peer-plane members and control-plane hosts
// are untouched.
func TestDropClusterJoinOnServerChange_OrphansPlainMember(t *testing.T) {
	on := true
	fc := &conf.FileConfig{
		AgentName: "worker",
		Cluster: &conf.ClusterConfig{
			Enabled:   &on,
			Runtimes:  conf.ClusterRuntimes{Agent: true},
			NodeName:  "worker-node",
			APIURL:    "https://old.example.com/api/cluster/agent",
			Token:     "old-bearer",
			NodeToken: "K10old::node:old",
		},
	}
	if !DropClusterJoinOnServerChange(fc, "https://old.example.com", "https://new.example.com") {
		t.Fatal("expected the server change to orphan the member")
	}
	if fc.ClusterOn() {
		t.Error("cluster mode stayed on across a server change (auto-join)")
	}
	if fc.Cluster.APIURL != "" || fc.Cluster.Token != "" || fc.Cluster.NodeToken != "" {
		t.Errorf("cached join survived: %+v", fc.Cluster)
	}
	if !fc.Cluster.Runtimes.Agent || fc.Cluster.NodeName != "worker-node" {
		t.Errorf("host-owned fields clobbered: %+v", fc.Cluster)
	}
}

func TestDropClusterJoinOnServerChange_SameServerKeepsJoin(t *testing.T) {
	on := true
	fc := &conf.FileConfig{
		Cluster: &conf.ClusterConfig{
			Enabled:   &on,
			APIURL:    "https://same.example.com/api/cluster/agent",
			Token:     "bearer",
			NodeToken: "K10::node:x",
		},
	}
	if DropClusterJoinOnServerChange(fc, "https://same.example.com", "https://same.example.com") {
		t.Error("same-server re-pair must not touch the join")
	}
	if !fc.ClusterOn() || fc.Cluster.APIURL == "" || fc.Cluster.NodeToken == "" {
		t.Errorf("same-server re-pair dropped the join: %+v", fc.Cluster)
	}
}

func TestDropClusterJoinOnServerChange_PeerAndControlPlaneUntouched(t *testing.T) {
	on := true
	peer := &conf.FileConfig{
		Cluster: &conf.ClusterConfig{
			Enabled: &on, JoinEndpoint: "10.0.0.5:7000", JoinToken: "t", NodeToken: "K10peer::node:p",
		},
	}
	if DropClusterJoinOnServerChange(peer, "https://old.example.com", "https://new.example.com") {
		t.Error("peer-plane member must keep its credentials")
	}
	if !peer.ClusterOn() || peer.Cluster.JoinEndpoint == "" {
		t.Errorf("peer-plane member touched: %+v", peer.Cluster)
	}
	cp := &conf.FileConfig{
		Cluster: &conf.ClusterConfig{Enabled: &on, ControlPlane: &on, NodeToken: "K10cp::node:c"},
	}
	if DropClusterJoinOnServerChange(cp, "https://old.example.com", "https://new.example.com") {
		t.Error("control-plane host must keep its credentials")
	}
	if !cp.ClusterOn() || cp.Cluster.NodeToken == "" {
		t.Errorf("control-plane host touched: %+v", cp.Cluster)
	}
}
