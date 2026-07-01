package mesh

import (
	"reflect"
	"testing"
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
