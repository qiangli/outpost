package mesh

import (
	"reflect"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
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
