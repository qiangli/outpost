package discovery

import (
	"slices"
	"strings"
	"testing"
)

// TestPeerID_IsValid covers the cheap syntactic check we use across
// the package boundary to validate fingerprint shape before doing
// anything with it.
func TestPeerID_IsValid(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"SHA256:Z6JEnskW1k5N2OFEcLmRpY+UDc/yX4tFr8r5KH8e0Dk", true},
		{"", false},
		{"SHA256:", false},
		{"shortprefix", false},
		{"MD5:abc", false},
	}
	for _, c := range cases {
		got := PeerID(c.in).IsValid()
		if got != c.want {
			t.Errorf("PeerID(%q).IsValid() = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestBuildTXTRecords confirms TXT serialization omits empty fields
// and keeps the standard key shape.
func TestBuildTXTRecords(t *testing.T) {
	opts := AdvertiseOptions{
		PeerID:           "SHA256:abc",
		AgentName:        "dragon",
		AssignedHostname: "dragon-7a3b",
		OSUsername:       "qiangli",
		OAuth2Email:      "", // empty: must be omitted
		CloudboxBase:     "https://ai.dhnt.io",
		Version:          "b49e182",
		Paired:           true,
		SSHListenAddr:    "0.0.0.0:2222",
	}
	got := buildTXTRecords(opts)
	mustContain(t, got, "id=SHA256:abc")
	mustContain(t, got, "an=dragon")
	mustContain(t, got, "host=dragon-7a3b")
	mustContain(t, got, "user=qiangli")
	mustContain(t, got, "cb=https://ai.dhnt.io")
	mustContain(t, got, "ver=b49e182")
	mustContain(t, got, "pair=1")
	mustContain(t, got, "ssh=0.0.0.0:2222")
	// OAuth2Email empty -> excluded; HTTPDiscoverListenAddr empty -> excluded.
	for _, r := range got {
		if strings.HasPrefix(r, "email=") {
			t.Errorf("empty OAuth2Email leaked into TXT: %q", r)
		}
		if strings.HasPrefix(r, "http=") {
			t.Errorf("empty HTTPDiscoverListenAddr leaked into TXT: %q", r)
		}
	}
}

// TestParseTXT round-trips the records we'd send via TXT back into
// the map shape the browse path uses to populate Peer fields.
func TestParseTXT(t *testing.T) {
	records := []string{
		"id=SHA256:abc",
		"an=dragon",
		"host=dragon-7a3b",
		"email=liqiang@gmail.com",
		"pair=1",
		"bare-key", // edge case: no `=`
	}
	m := parseTXT(records)
	if m["id"] != "SHA256:abc" {
		t.Errorf("id=%q want SHA256:abc", m["id"])
	}
	if m["email"] != "liqiang@gmail.com" {
		t.Errorf("email=%q", m["email"])
	}
	if v, ok := m["bare-key"]; !ok || v != "" {
		t.Errorf("bare-key parse: got %q ok=%v want empty/true", v, ok)
	}
}

// TestSplitHostPortLoose accepts the three shapes the browse path
// has to handle without surprise.
func TestSplitHostPortLoose(t *testing.T) {
	cases := []struct {
		in       string
		wantHost string
		wantPort int
	}{
		{"127.0.0.1:8080", "127.0.0.1", 8080},
		{":2222", "", 2222},
		{"2222", "", 2222},
		{"http://example.com:80", "example.com", 80},
		{"", "", 0},
		{"x:notaport", "x", 0},
		{":99999", "", 0}, // out of range
	}
	for _, c := range cases {
		host, port := splitHostPortLoose(c.in)
		if host != c.wantHost || port != c.wantPort {
			t.Errorf("splitHostPortLoose(%q) = (%q, %d), want (%q, %d)",
				c.in, host, port, c.wantHost, c.wantPort)
		}
	}
}

// TestPeer_AddSource covers the idempotent Source-set behavior used
// when the same peer is rediscovered via multiple channels.
func TestPeer_AddSource(t *testing.T) {
	p := Peer{}
	p.AddSource(SourceMDNS)
	p.AddSource(SourceHTTPProbe)
	p.AddSource(SourceMDNS) // duplicate
	if len(p.Sources) != 2 {
		t.Fatalf("Sources length = %d, want 2: %v", len(p.Sources), p.Sources)
	}
	if !slices.Contains(p.Sources, SourceMDNS) || !slices.Contains(p.Sources, SourceHTTPProbe) {
		t.Errorf("expected both mdns and http-probe in Sources: %v", p.Sources)
	}
}

func mustContain(t *testing.T, recs []string, want string) {
	t.Helper()
	if !slices.Contains(recs, want) {
		t.Errorf("TXT records missing %q: %v", want, recs)
	}
}
