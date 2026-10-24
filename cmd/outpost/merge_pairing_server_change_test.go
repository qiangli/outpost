package main

import (
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// S482 — a re-pair (register / recover) onto a DIFFERENT server used to keep
// the previous server's cached cluster join (api_url / token / ca): the merge
// starts from the on-disk block and overlays only server-owned membership
// fields, and none of those is the join triple. The host then kept dialing
// the old server's apiserver until the operator ran `outpost cluster clear`
// + `outpost cluster join`.
//
// S498.4 hardened the policy: dropping the join is not enough, because the
// next boot re-fetched a fresh one from the NEW server and the host silently
// auto-joined its cluster (a third-party host re-paired to another owner's
// satellite silently joined its cluster). A re-pair onto a different server
// now lands the host as an ORPHAN — join dropped AND cluster mode disabled,
// with no new-server membership applied. These tests pin the policy:
//
//   - server changed, plain server-plane member → orphan (no auto-join)
//   - same server (replacement instance at the same address) → join kept
//   - peer-plane member → its own credentials are never touched

func onDiskJoinedTo(server string) *conf.FileConfig {
	on := true
	return &conf.FileConfig{
		AgentName:  "worker",
		ServerAddr: server,
		ServerPort: 443,
		Protocol:   "wss",
		Cluster: &conf.ClusterConfig{
			Enabled:   &on,
			Runtimes:  conf.ClusterRuntimes{Agent: true},
			NodeName:  "worker-node",
			APIURL:    "https://" + server + "/api/cluster/agent",
			Token:     "old-bearer",
			CA:        []byte("-----BEGIN CERTIFICATE-----\nold\n-----END CERTIFICATE-----\n"),
			NodeToken: "K10old::node:old",
		},
	}
}

func exchangedFrom(server string) *conf.FileConfig {
	ex := exchangedWithCluster()
	ex.ServerAddr = server
	return ex
}

// S498.4: a re-pair onto a different server lands a plain server-plane
// member as an ORPHAN — the cached join is dropped, cluster mode is
// disabled, and the new server's membership is NOT applied, so no boot
// can silently join the new server's cluster. Only an explicit
// `outpost cluster join` rejoins.
func TestMergePairingServerChangeDropsCachedClusterJoin(t *testing.T) {
	path := writeConfig(t, onDiskJoinedTo("old.example.com"))
	merged := mergePairing(path, exchangedFrom("new.example.com"))
	cc := merged.Cluster
	if cc == nil {
		t.Fatal("cluster block dropped entirely")
	}
	if cc.APIURL != "" || cc.Token != "" || len(cc.CA) != 0 {
		t.Errorf("cached join survived a re-pair to a different server: api_url=%q token=%q ca=%d bytes",
			cc.APIURL, cc.Token, len(cc.CA))
	}
	// Orphan: cluster mode disabled, and no membership from the new server
	// was applied — nothing here can join a cluster without an explicit join.
	if merged.ClusterOn() {
		t.Errorf("re-pair to a different server left cluster mode on (auto-join): %+v", cc)
	}
	if cc.NodeToken != "" || cc.STCPSecret != "" || cc.K8sAPIPort != 0 ||
		cc.OverlayLoginServer != "" || cc.OverlayAuthKey != "" || cc.OverlayPodCIDR != "" {
		t.Errorf("new server's membership was issued to an orphaned host: %+v", cc)
	}
	// Host-authoritative non-membership fields stay: the runtime selection
	// and node name are not the old server's to lose, so an explicit rejoin
	// comes back as the same kind of node.
	if !cc.Runtimes.Agent || cc.NodeName != "worker-node" {
		t.Errorf("host-owned cluster fields clobbered: %+v", cc)
	}
	if merged.ServerAddr != "new.example.com" {
		t.Errorf("pairing not applied: %+v", merged)
	}
}

func TestMergePairingSameServerKeepsClusterJoin(t *testing.T) {
	path := writeConfig(t, onDiskJoinedTo("same.example.com"))
	merged := mergePairing(path, exchangedFrom("same.example.com"))
	cc := merged.Cluster
	if cc.APIURL != "https://same.example.com/api/cluster/agent" || cc.Token != "old-bearer" || len(cc.CA) == 0 {
		t.Errorf("same-server re-pair dropped the cached join: %+v", cc)
	}
}

func TestMergePairingPeerPlaneKeepsCreds(t *testing.T) {
	fc := onDiskJoinedTo("old.example.com")
	fc.Cluster.JoinEndpoint = "10.0.0.5:7000"
	fc.Cluster.JoinToken = "peer-tunnel-token"
	fc.Cluster.NodeToken = "K10peer::node:peer"
	fc.Cluster.STCPSecret = "peer-secret"
	path := writeConfig(t, fc)
	merged := mergePairing(path, exchangedFrom("new.example.com"))
	cc := merged.Cluster
	if cc.JoinEndpoint != "10.0.0.5:7000" || cc.JoinToken != "peer-tunnel-token" {
		t.Errorf("peer join dropped: %+v", cc)
	}
	if cc.NodeToken != "K10peer::node:peer" || cc.STCPSecret != "peer-secret" {
		t.Errorf("peer credentials clobbered: %+v", cc)
	}
	if cc.APIURL != "https://old.example.com/api/cluster/agent" || cc.Token != "old-bearer" || len(cc.CA) == 0 {
		t.Errorf("peer-plane member lost its join triple on a server change: %+v", cc)
	}
}
