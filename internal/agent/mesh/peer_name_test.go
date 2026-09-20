package mesh

import (
	"testing"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/host/peerstore/pstoremem"
)

// The neighbour name comes from the peer's own announced user-agent, so a
// LAN neighbour list needs no cloudbox (sprint 220, story ed857a89).
func TestPeerNameFromAgent(t *testing.T) {
	ps, err := pstoremem.NewPeerstore()
	if err != nil {
		t.Fatal(err)
	}
	defer ps.Close()
	pid, err := peer.Decode("12D3KooWRRq3Lxgb4rtb3VyptQj9RaaQT41h5VjioVpNs3BNdgUs")
	if err != nil {
		t.Fatal(err)
	}
	if got := peerNameFromAgent(ps, pid); got != "" {
		t.Fatalf("unknown peer named %q", got)
	}
	_ = ps.Put(pid, "AgentVersion", "outpost-mesh/winbox")
	if got := peerNameFromAgent(ps, pid); got != "winbox" {
		t.Fatalf("name = %q, want winbox", got)
	}
	_ = ps.Put(pid, "AgentVersion", "go-libp2p/0.41")
	if got := peerNameFromAgent(ps, pid); got != "" {
		t.Fatalf("a non-outpost peer got a name: %q", got)
	}
}
