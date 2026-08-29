package mesh

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"

	"github.com/qiangli/outpost/internal/agent/peerplane"
	"github.com/qiangli/outpost/internal/agent/peerstatus"
)

// TestPeersToRedial verifies the sweep selects exactly the known owned/shared
// peers that libp2p reports NOT connected, skips connected ones and blank
// entries, and returns them deterministically ordered by host.
func TestPeersToRedial(t *testing.T) {
	hostPeers := map[string]string{
		"node-c": "pid-c", // disconnected → redial
		"node-a": "pid-a", // connected     → skip
		"node-b": "pid-b", // disconnected → redial
		"node-d": "",      // no peer id     → skip
		"":       "pid-x", // no host        → skip
	}
	connected := map[string]bool{"pid-a": true}

	got := peersToRedial(hostPeers, func(pid string) bool { return connected[pid] })

	want := []redialTarget{
		{Host: "node-b", PeerID: "pid-b"},
		{Host: "node-c", PeerID: "pid-c"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("peersToRedial = %+v, want %+v", got, want)
	}
}

// TestPeersToRedialAllConnected verifies a fully-connected fleet yields no
// redial work (the common steady-state — the sweep must be a cheap no-op).
func TestPeersToRedialAllConnected(t *testing.T) {
	hostPeers := map[string]string{"node-a": "pid-a", "node-b": "pid-b"}
	got := peersToRedial(hostPeers, func(string) bool { return true })
	if len(got) != 0 {
		t.Fatalf("expected no redial targets, got %+v", got)
	}
}

// TestSweepBackoff exercises the per-peer reconnect-sweep backoff with an
// explicit fake clock: a fresh peer is eligible and takes the min backoff; a
// re-dial soon after grows the backoff and blocks the peer until nextEligible;
// a re-dial long after resets to the min.
func TestSweepBackoff(t *testing.T) {
	r := &Rendezvous{dialState: make(map[peer.ID]*peerDial)}
	pid := peer.ID("pid-a")
	base := time.Unix(1_700_000_000, 0)

	// (a) Fresh pid: eligible, then noteDial sets min backoff + nextEligible.
	if !r.sweepEligible(pid, base) {
		t.Fatal("fresh peer should be eligible")
	}
	r.noteDial(pid, base)
	st := r.dialState[pid]
	if st == nil {
		t.Fatal("noteDial did not create a dial record")
	}
	if st.backoff != sweepBackoffMin {
		t.Fatalf("fresh backoff = %v, want %v", st.backoff, sweepBackoffMin)
	}
	if !st.nextEligible.Equal(base.Add(sweepBackoffMin)) {
		t.Fatalf("nextEligible = %v, want %v", st.nextEligible, base.Add(sweepBackoffMin))
	}
	if r.sweepEligible(pid, base) {
		t.Fatal("peer should be ineligible immediately after noteDial")
	}

	// (b) A second noteDial a short time later (< 3× backoff) grows to 2× min
	// and pushes nextEligible out; ineligible until now >= nextEligible.
	soon := base.Add(sweepBackoffMin) // exactly at nextEligible → eligible again
	if !r.sweepEligible(pid, soon) {
		t.Fatal("peer should be eligible once nextEligible has passed")
	}
	r.noteDial(pid, soon)
	if st.backoff != 2*sweepBackoffMin {
		t.Fatalf("grown backoff = %v, want %v", st.backoff, 2*sweepBackoffMin)
	}
	if !st.nextEligible.Equal(soon.Add(2 * sweepBackoffMin)) {
		t.Fatalf("nextEligible = %v, want %v", st.nextEligible, soon.Add(2*sweepBackoffMin))
	}
	// Still inside the grown window → ineligible.
	if r.sweepEligible(pid, soon.Add(sweepBackoffMin)) {
		t.Fatal("peer should be ineligible inside the grown backoff window")
	}
	// Past the grown window → eligible.
	if !r.sweepEligible(pid, soon.Add(2*sweepBackoffMin)) {
		t.Fatal("peer should be eligible once the grown window elapses")
	}

	// (c) A noteDial long after the last attempt (> 3× backoff) resets to min.
	// Current backoff is 2×min; wait > 3× that before dialing again.
	late := st.lastDialAt.Add(3*st.backoff + time.Second)
	r.noteDial(pid, late)
	if st.backoff != sweepBackoffMin {
		t.Fatalf("reset backoff = %v, want %v", st.backoff, sweepBackoffMin)
	}
	if !st.nextEligible.Equal(late.Add(sweepBackoffMin)) {
		t.Fatalf("nextEligible = %v, want %v", st.nextEligible, late.Add(sweepBackoffMin))
	}
}

// TestSweepBackoffCap verifies the backoff saturates at sweepBackoffMax under
// repeated soon-after re-dials rather than growing without bound.
func TestSweepBackoffCap(t *testing.T) {
	r := &Rendezvous{dialState: make(map[peer.ID]*peerDial)}
	pid := peer.ID("pid-flap")
	now := time.Unix(1_700_000_000, 0)
	r.noteDial(pid, now)
	st := r.dialState[pid]
	// Re-dial repeatedly just inside the current window so each attempt doubles.
	for i := 0; i < 20; i++ {
		now = st.nextEligible // eligible again, still within 3× backoff of lastDialAt
		r.noteDial(pid, now)
	}
	if st.backoff != sweepBackoffMax {
		t.Fatalf("saturated backoff = %v, want %v", st.backoff, sweepBackoffMax)
	}
}

// TestParseMultiaddrs verifies malformed candidate strings are dropped and
// valid ones (whitespace-trimmed) are kept.
func TestParseMultiaddrs(t *testing.T) {
	in := []string{
		" /ip4/127.0.0.1/tcp/4001 ",
		"not-a-multiaddr",
		"",
		"/ip4/10.0.0.5/udp/4001/quic-v1",
	}
	got := parseMultiaddrs(in)
	if len(got) != 2 {
		t.Fatalf("parseMultiaddrs kept %d addrs, want 2: %v", len(got), got)
	}
}

// TestDiscoverAndDialResolvesAlias is the regression for the seventh
// silent-downgrade defect of sprint 84: cloudbox files a paired host under
// BOTH its registered name and an alias (`outpost peers status` prints
// `dog (novidesign)`), but the rendezvous learned the peer id only under the
// registered name. `outpost reach dog` then missed the mesh lookup and was
// downgraded to the cloudbox relay while `outpost reach novidesign` took the
// direct link — same host, opposite verdict.
//
// The test drives the real discovery path (discoverAndDial against a fake
// cloudbox + a real libp2p peer) and asserts that both spellings resolve to
// the same peer id. Before the fix the alias lookup returned "".
func TestDiscoverAndDialResolvesAlias(t *testing.T) {
	const (
		registered = "novidesign"
		alias      = "dog"
	)

	self := newTestHost(t)
	defer self.Close()
	peerHost := newTestHost(t)
	defer peerHost.Close()

	peerAddrs := make([]string, 0, len(peerHost.LibP2PHost().Addrs()))
	for _, a := range peerHost.LibP2PHost().Addrs() {
		peerAddrs = append(peerAddrs, a.String())
	}

	// Fake cloudbox: the peer listing carries both spellings; connect answers
	// only for the REGISTERED name (that is what discoverAndDial sends).
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/peers", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"peers": []peerstatus.Peer{
				{Host: registered, Alias: alias, Owned: true, Online: true},
			},
		})
	})
	mux.HandleFunc("/api/v1/peer/connect", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["to_host"] != registered {
			http.Error(w, "unknown host "+in["to_host"], http.StatusNotFound)
			return
		}
		var out peerplane.PeerTarget
		out.Peer.Host = registered
		out.Peer.PeerID = peerHost.PeerID()
		out.Peer.Candidates = peerAddrs
		_ = json.NewEncoder(w).Encode(out)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	r := NewRendezvous(self, "self", srv.URL, "tok", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	r.discoverAndDial(ctx)

	if got := r.PeerIDForHost(registered); got != peerHost.PeerID() {
		t.Fatalf("PeerIDForHost(%q) = %q, want %q (registered name must resolve)", registered, got, peerHost.PeerID())
	}
	// The acceptance criterion: the alias resolves to the SAME peer id as the
	// registered name, so `reach <alias>` and `reach <registered>` classify
	// the same route.
	if got := r.PeerIDForHost(alias); got != peerHost.PeerID() {
		t.Fatalf("PeerIDForHost(%q) = %q, want %q (alias silently missed the mesh lookup)", alias, got, peerHost.PeerID())
	}
	if got := r.PeerIDForHost(strings.ToUpper(alias)); got != peerHost.PeerID() {
		t.Fatalf("PeerIDForHost(%q) = %q, want %q (alias match must be case-insensitive)", strings.ToUpper(alias), got, peerHost.PeerID())
	}
	if got := r.CanonicalHost(alias); got != registered {
		t.Fatalf("CanonicalHost(%q) = %q, want %q", alias, got, registered)
	}
	if got := r.PeerIDForHost("nobody"); got != "" {
		t.Fatalf("PeerIDForHost(unknown) = %q, want empty", got)
	}
	// LinkInfoForHost rides the same key: both spellings report the same link.
	if a, b := r.LinkInfoForHost(alias), r.LinkInfoForHost(registered); a != b {
		t.Fatalf("LinkInfoForHost differs by spelling: alias=%+v registered=%+v", a, b)
	}
}

// TestObservePeersAliasRules pins the alias-table edge cases: an alias equal
// to the registered name is dropped, a renamed alias replaces the old mapping,
// blanks are ignored, and an unknown name falls through unchanged so it still
// misses the lookup honestly.
func TestObservePeersAliasRules(t *testing.T) {
	r := NewRendezvous(nil, "self", "http://unused", "", nil)
	r.rememberPeer("novidesign", "pid-n")
	r.rememberPeer("Mixed-Case", "pid-m")

	r.observePeers([]peerstatus.Peer{
		{Host: "novidesign", Alias: "dog"},
		{Host: "plain", Alias: "plain"},
		{Host: "", Alias: "orphan"},
		{Host: "blank", Alias: "  "},
	})
	if got := r.PeerIDForHost("dog"); got != "pid-n" {
		t.Fatalf("alias dog -> %q, want pid-n", got)
	}
	if got := r.CanonicalHost("plain"); got != "plain" {
		t.Fatalf("self-alias should be a no-op, got %q", got)
	}
	if got := r.CanonicalHost("orphan"); got != "orphan" {
		t.Fatalf("alias with no host must not map, got %q", got)
	}
	if got := r.PeerIDForHost("mixed-case"); got != "pid-m" {
		t.Fatalf("registered name should match case-insensitively, got %q", got)
	}

	// Alias renamed cloud-side: the new spelling wins, the old one is gone
	// only if cloudbox stops reporting it — mappings are additive per tick,
	// mirroring hostPeers, so the stale key is still a hit until then. What
	// must hold is that the NEW alias resolves.
	r.observePeers([]peerstatus.Peer{{Host: "novidesign", Alias: "puppy"}})
	if got := r.PeerIDForHost("puppy"); got != "pid-n" {
		t.Fatalf("renamed alias puppy -> %q, want pid-n", got)
	}
}
