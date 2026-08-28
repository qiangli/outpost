package admincore

import "testing"

// MeshHostLink is what lets the reachability ladder prefer an existing
// peer-to-peer link over the cloudbox relay. The cases that matter are the
// negative ones: a relayed circuit or a stale peer id must NOT be reported as
// a direct route, because outranking cloudbox on a path that cannot carry
// traffic is worse than the relay we are trying to avoid.
func TestMeshHostLink(t *testing.T) {
	const peerA = "12D3KooWpeerA"

	status := func() *MeshStatusView {
		return &MeshStatusView{
			PeerID:         "12D3KooWself",
			ConnectedPeers: 2,
			Peers: []MeshPeerConnView{
				{ID: peerA, Direct: true, LinkClass: "lan", Remote: []string{"/ip4/10.0.0.5/udp/1/quic-v1"}},
				{ID: "12D3KooWrelayed", Direct: false, LinkClass: "wan"},
			},
		}
	}

	for _, tc := range []struct {
		name       string
		host       string
		peerByHost func(string) string
		meshStatus func() *MeshStatusView
		wantFound  bool
		wantDirect bool
	}{
		{
			name: "direct link is found and direct",
			host: "host-a", meshStatus: status,
			peerByHost: func(string) string { return peerA },
			wantFound:  true, wantDirect: true,
		},
		{
			name: "relayed circuit is found but NOT direct",
			host: "host-r", meshStatus: status,
			peerByHost: func(string) string { return "12D3KooWrelayed" },
			wantFound:  true, wantDirect: false,
		},
		{
			name: "known peer id that is not connected is not direct",
			host: "host-gone", meshStatus: status,
			peerByHost: func(string) string { return "12D3KooWnotconnected" },
			wantFound:  true, wantDirect: false,
		},
		{
			name: "unknown host",
			host: "nobody", meshStatus: status,
			peerByHost: func(string) string { return "" },
			wantFound:  false, wantDirect: false,
		},
		{
			name: "mesh data plane off (nil resolver)",
			host: "host-a", meshStatus: status,
			peerByHost: nil,
			wantFound:  false, wantDirect: false,
		},
		{
			name:       "mesh status unavailable",
			host:       "host-a",
			meshStatus: func() *MeshStatusView { return nil },
			peerByHost: func(string) string { return peerA },
			wantFound:  false, wantDirect: false,
		},
		{
			name: "empty host is never a route",
			host: "", meshStatus: status,
			peerByHost: func(string) string { return peerA },
			wantFound:  false, wantDirect: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := New(Deps{
				ConfigPath:       t.TempDir() + "/agent.json",
				MeshStatus:       tc.meshStatus,
				MeshPeerIDByHost: tc.peerByHost,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			got := s.MeshHostLink(tc.host)
			if got.Found != tc.wantFound {
				t.Errorf("Found = %v, want %v", got.Found, tc.wantFound)
			}
			if got.Direct != tc.wantDirect {
				t.Errorf("Direct = %v, want %v", got.Direct, tc.wantDirect)
			}
			if tc.wantDirect && got.LinkClass == "" {
				t.Error("a direct link should carry its link class")
			}
		})
	}
}

// The mesh rung must never be reported as a route when the daemon has no mesh
// at all — otherwise reach would claim a direct path on a host that has none.
func TestMeshHostLinkNoMeshDeps(t *testing.T) {
	s, err := New(Deps{ConfigPath: t.TempDir() + "/agent.json"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := s.MeshHostLink("anything"); got.Found || got.Direct {
		t.Fatalf("no-mesh daemon reported a link: %+v", got)
	}
}
