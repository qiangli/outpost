package discovery

import (
	"context"
	"slices"
	"testing"
	"time"
)

// TestGossipPushPullExchange pins the core gossip behavior: two
// outposts on loopback exchange cache state over the gossip listener,
// fresh peers propagate (SourceGossip, trust downgraded to
// Unverified), stale peers do not, and each side tracks the other as
// a live member.
func TestGossipPushPullExchange(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const idA = "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	const idB = "SHA256:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	const idX = "SHA256:XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX"
	const idY = "SHA256:YYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYY"

	// A knows fresh peer X and long-stale peer Y.
	cacheA := NewCache(0)
	cacheA.Upsert(Peer{
		ID:   idX,
		Endpoints: []Endpoint{{Kind: EndpointLANSSH, Host: "192.0.2.10", Port: 2222}},
		Sources:   []Source{SourceMDNS},
		Trust:     TrustCloudboxCert, // must be downgraded to Unverified by the receiver
		LastSeenAt: time.Now(),
	})
	cacheA.Upsert(Peer{
		ID:   idY,
		Sources:   []Source{SourceMDNS},
		Trust:     TrustTOFU,
		LastSeenAt: time.Now().Add(-2 * gossipBroadcastTTL), // stale: must not propagate
	})

	gA, err := NewGossip(GossipConfig{
		SelfPeerID:    idA,
		SelfAgentName: "gossip-a",
		Cache:         cacheA,
		BindAddr:      "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("NewGossip(A): %v", err)
	}
	go func() { _ = gA.Run(ctx) }()

	addrA := waitNonEmpty(t, func() string { return gA.Addr() }, 5*time.Second)
	t.Logf("gossip A listening on %s", addrA)

	// B starts empty and bootstraps to A's listener.
	cacheB := NewCache(0)
	gB, err := NewGossip(GossipConfig{
		SelfPeerID:        idB,
		SelfAgentName:     "gossip-b",
		Cache:             cacheB,
		BindAddr:          "127.0.0.1:0",
		PushPullInterval:  200 * time.Millisecond,
		Bootstrap:         func() []string { return []string{addrA} },
	})
	if err != nil {
		t.Fatalf("NewGossip(B): %v", err)
	}
	go func() { _ = gB.Run(ctx) }()

	// B's cache must absorb X (and only X) with gossip semantics.
	deadline := time.Now().Add(5 * time.Second)
	for {
		snap := cacheB.Snapshot()
		if slices.ContainsFunc(snap, func(p Peer) bool { return p.ID == idX }) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("B never received peer X via gossip; cache=%v", snap)
		}
		time.Sleep(50 * time.Millisecond)
	}
	var gotX Peer
	for _, p := range cacheB.Snapshot() {
		if p.ID == idY {
			t.Errorf("stale peer Y propagated via gossip: %+v", p)
		}
		if p.ID == idX {
			gotX = p
		}
	}
	if !slices.Contains(gotX.Sources, SourceGossip) {
		t.Errorf("X missing %q source: %v", SourceGossip, gotX.Sources)
	}
	if gotX.Trust != TrustUnverified {
		t.Errorf("X trust = %q, want %q (third-party gossip must downgrade)", gotX.Trust, TrustUnverified)
	}

	// Both sides must list each other as alive members.
	memB := waitMembers(t, gB, idA, 5*time.Second)
	if memB.State != "alive" {
		t.Errorf("B sees A as %q, want alive (member=%+v)", memB.State, memB)
	}
	memA := waitMembers(t, gA, idB, 5*time.Second)
	if memA.State != "alive" {
		t.Errorf("A sees B as %q, want alive (member=%+v)", memA.State, memA)
	}

	// Members never includes self.
	for _, m := range gA.Members() {
		if m.PeerID == idA {
			t.Errorf("A's Members() includes self: %+v", m)
		}
	}
}

func waitNonEmpty(t *testing.T, fn func() string, within time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if s := fn(); s != "" {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for gossip listener address")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func waitMembers(t *testing.T, g *Gossip, id PeerID, within time.Duration) GossipMember {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		for _, m := range g.Members() {
			if m.PeerID == id {
				return m
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for member %s in %v", id, g.Members())
		}
		time.Sleep(50 * time.Millisecond)
	}
}
