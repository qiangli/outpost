package mesh

import (
	"context"
	"crypto/rand"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

func TestLoadOrCreateKeyPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mesh_ed25519")

	k1, err := loadOrCreateKeyAt(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	k2, err := loadOrCreateKeyAt(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !k1.Equals(k2) {
		t.Fatal("key not stable across reloads — peer ID would churn on restart")
	}
}

// TestHostsConnectDirect stands up two mesh hosts on ephemeral ports and
// verifies a direct dial forms an authenticated connection — the loopback
// equivalent of the peer↔peer link the mesh provides over the network.
func TestHostsConnectDirect(t *testing.T) {
	h1 := newTestHost(t)
	defer h1.Close()
	h2 := newTestHost(t)
	defer h2.Close()

	if h1.PeerID() == h2.PeerID() {
		t.Fatal("distinct identities should yield distinct peer IDs")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	ai := peer.AddrInfo{ID: h2.LibP2PHost().ID(), Addrs: h2.LibP2PHost().Addrs()}
	if err := h1.LibP2PHost().Connect(ctx, ai); err != nil {
		t.Fatalf("connect h1->h2: %v", err)
	}

	if got := h1.Status().ConnectedPeers; got != 1 {
		t.Fatalf("h1 connected peers = %d, want 1", got)
	}
	if len(h1.Status().ListenAddrs) == 0 {
		t.Fatal("host should report listen addrs")
	}
}

// TestStatusPeerDetail verifies Status() carries per-connected-peer link
// detail: after a direct dial the connected peer shows Direct=true, at least
// one Remote multiaddr, and a LinkClass consistent with the strongest class of
// those remote addrs (the test host advertises whatever interface addrs it has,
// so we assert the wiring is consistent rather than a fixed class — the
// classification of specific link-local / LAN / WAN addrs is covered by the
// linkclass table tests).
func TestStatusPeerDetail(t *testing.T) {
	h1 := newTestHost(t)
	defer h1.Close()
	h2 := newTestHost(t)
	defer h2.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	ai := peer.AddrInfo{ID: h2.LibP2PHost().ID(), Addrs: h2.LibP2PHost().Addrs()}
	if err := h1.LibP2PHost().Connect(ctx, ai); err != nil {
		t.Fatalf("connect h1->h2: %v", err)
	}

	st := h1.Status()
	if len(st.Peers) != 1 {
		t.Fatalf("Status().Peers = %d entries, want 1", len(st.Peers))
	}
	pc := st.Peers[0]
	if pc.ID != h2.PeerID() {
		t.Errorf("peer id = %q, want %q", pc.ID, h2.PeerID())
	}
	if !pc.Direct {
		t.Error("a non-relayed dial should be reported Direct=true")
	}
	if len(pc.Remote) == 0 {
		t.Fatal("peer detail should carry at least one remote multiaddr")
	}
	// LinkClass must equal the strongest class derived from the reported
	// remote addrs — proving the detail-building wiring (not just the count).
	want := ""
	for _, r := range pc.Remote {
		m, err := ma.NewMultiaddr(r)
		if err != nil {
			t.Fatalf("remote %q is not a valid multiaddr: %v", r, err)
		}
		want = strongerLinkClass(want, classifyConnAddr(m))
	}
	if pc.LinkClass != want {
		t.Errorf("link class = %q, want %q (derived from remote addrs %v)", pc.LinkClass, want, pc.Remote)
	}
}

// dialableAddrs is what we announce to cloudbox for peers to dial back — it
// must never include an unspecified (0.0.0.0 / ::) listen address.
func TestDialableAddrsNoUnspecified(t *testing.T) {
	h := newTestHost(t)
	defer h.Close()
	for _, a := range h.dialableAddrs() {
		if strings.Contains(a, "/0.0.0.0/") || strings.Contains(a, "/::/") {
			t.Errorf("dialableAddrs leaked an unspecified addr: %s", a)
		}
	}
}

func newTestHost(t *testing.T) *Host {
	t.Helper()
	priv, _, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(Config{ListenPort: 0, PrivKey: priv, DisableMDNS: true})
	if err != nil {
		t.Fatalf("new host: %v", err)
	}
	return h
}

func newTestHostWithRelay(t *testing.T, relayAddr string) *Host {
	t.Helper()
	priv, _, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(Config{ListenPort: 0, PrivKey: priv, RelayAddrs: []string{relayAddr}, DisableMDNS: true})
	if err != nil {
		t.Fatalf("new host with relay: %v", err)
	}
	return h
}
