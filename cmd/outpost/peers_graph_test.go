package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/qiangli/outpost/internal/agent/peerstatus"
	"github.com/qiangli/yoke/pkg/lens/stream"
)

func TestPeersStatusGraphStream(t *testing.T) {
	peers := []peerstatus.Peer{
		{Host: "zeta", Alias: "desk", Shared: true, Online: true, Location: "same_lan", OS: "linux", Arch: "amd64"},
		{Host: "alpha", Owned: true, Location: "remote", Version: "1.2"},
	}
	encode := func(ps []peerstatus.Peer) string {
		t.Helper()
		var b bytes.Buffer
		if err := writePeersStatusGraph(&b, ps); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	first := encode(peers)
	if again := encode([]peerstatus.Peer{peers[1], peers[0]}); first != again {
		t.Fatalf("response order changed the stream:\n%s\nversus\n%s", first, again)
	}
	graph, err := stream.Decode(strings.NewReader(first), stream.DecodeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !graph.Framed || graph.Source != peersStatusSource || len(graph.Nodes) != 3 || len(graph.Edges) != 2 {
		t.Fatalf("unexpected graph: %+v", graph)
	}
	if graph.Cursor == nil || graph.Cursor.Binding.Provider != "outpost" || graph.Cursor.Binding.Revision != graph.Rev {
		t.Fatalf("unbound cursor: %+v", graph.Cursor)
	}
	if graph.Nodes[1].Ref != "peer:alpha" || graph.Nodes[2].Ref != "peer:zeta" {
		t.Fatalf("peer addresses: %+v", graph.Nodes)
	}
	if graph.Nodes[2].Attrs["online"] != true || graph.Nodes[2].Attrs["alias"] != "desk" {
		t.Fatalf("peer status lost: %+v", graph.Nodes[2].Attrs)
	}
	if !strings.Contains(first, `"rel":"contains"`) {
		t.Fatalf("roster relation missing: %s", first)
	}
}

func TestPeersStatusGraphRejectsUnaddressablePeer(t *testing.T) {
	var b bytes.Buffer
	if err := writePeersStatusGraph(&b, []peerstatus.Peer{{Online: true}}); err == nil {
		t.Fatal("missing host accepted")
	}
	if b.Len() != 0 {
		t.Fatal("partial stream written")
	}
}
