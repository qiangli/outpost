package peerplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
)

// TestCycleCoAnnouncesMeshIdentity pins the fix for the flapping mesh service
// registry: cloudbox keeps one PeerNode row per host and overwrites peer_id +
// candidates on every announce, so the RTT prober must publish the mesh peer
// id + the union of candidates (never a blank peer id) or it erases what the
// mesh rendezvous announced and /peer/resolve drops the host.
func TestCycleCoAnnouncesMeshIdentity(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/peer/announce" {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			got = body
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
			return
		}
		http.Error(w, "not in this test", http.StatusInternalServerError)
	}))
	defer srv.Close()

	s := New(Config{AgentName: "dragon", CloudboxURL: srv.URL, AccessToken: "tok", HTTPClient: srv.Client()})

	// Without a mesh identity provider the prober announces as before:
	// blank peer id, its own probe candidates only.
	s.cycle(context.Background(), 17)
	mu.Lock()
	first := got
	mu.Unlock()
	if first == nil {
		t.Fatal("no announce reached the fake cloudbox")
	}
	if first["peer_id"] != "" {
		t.Fatalf("peer_id without identity = %q, want empty", first["peer_id"])
	}
	if _, has := first["services"]; has {
		t.Fatalf("prober must not send services (would clobber the mesh's list): %v", first)
	}

	// With the mesh identity installed the announce carries the peer id and
	// the union of mesh multiaddrs + probe candidates.
	meshAddrs := []string{"/ip4/10.0.0.7/udp/4001/quic-v1", "/ip4/10.0.0.7/tcp/4001"}
	s.SetIdentity(func() (string, []string) { return "12D3KooWmesh", meshAddrs })
	s.cycle(context.Background(), 17)
	mu.Lock()
	second := got
	mu.Unlock()
	if second["peer_id"] != "12D3KooWmesh" {
		t.Fatalf("peer_id = %q, want mesh peer id", second["peer_id"])
	}
	cands, _ := second["candidates"].(string)
	want := MergeCandidates(meshAddrs, LocalCandidates(17))
	if cands != joinCSV(want) {
		t.Fatalf("candidates = %q, want %q", cands, joinCSV(want))
	}
	if _, has := second["services"]; has {
		t.Fatalf("prober must not send services: %v", second)
	}
}

func joinCSV(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return out
}

func TestCandidatesEmptyUntilRun(t *testing.T) {
	s := New(Config{AgentName: "x"})
	if c := s.Candidates(); len(c) != 0 {
		t.Fatalf("Candidates before Run = %v, want none", c)
	}
	s.mu.Lock()
	s.port = 4242
	s.mu.Unlock()
	if got, want := s.Candidates(), LocalCandidates(4242); !reflect.DeepEqual(got, want) {
		t.Fatalf("Candidates = %v, want %v", got, want)
	}
}

func TestMergeCandidates(t *testing.T) {
	got := MergeCandidates(
		[]string{"/ip4/1.2.3.4/tcp/1", " ", "10.0.0.1:5"},
		[]string{"10.0.0.1:5", "10.0.0.2:5", ""},
		nil,
	)
	want := []string{"/ip4/1.2.3.4/tcp/1", "10.0.0.1:5", "10.0.0.2:5"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MergeCandidates = %v, want %v", got, want)
	}
}

// TestProbeCandidatesSkipsMultiaddrs: the shared row now carries libp2p
// multiaddrs too; the UDP prober must not burn a timeout on each of them.
func TestProbeCandidatesSkipsMultiaddrs(t *testing.T) {
	got := probeCandidates([]string{"/ip4/10.0.0.7/udp/4001/quic-v1", "10.0.0.7:9000", "", " 10.0.0.8:9000 "})
	want := []string{"10.0.0.7:9000", "10.0.0.8:9000"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("probeCandidates = %v, want %v", got, want)
	}
}
