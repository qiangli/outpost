package main

import (
	"fmt"
	"io"

	"github.com/qiangli/outpost/internal/agent/peerstatus"
	"github.com/qiangli/yoke/pkg/lens/provider"
	"github.com/qiangli/yoke/pkg/lens/stream"
	"github.com/qiangli/yoke/pkg/ref"
)

const peersStatusSource = "graph:outpost-peers"

// writePeersStatusGraph describes the visible peer roster without a timestamp.
// The provider supplies one status snapshot; its contents determine the stream
// revision. The encoder orders peers by ref, independently of API response order.
func writePeersStatusGraph(w io.Writer, peers []peerstatus.Peer) error {
	b := stream.NewBuilder(peersStatusSource)
	b.Node(provider.Node{Ref: peersStatusSource, Name: "visible peers", Kind: "roster", Type: "outpost/peers"})
	for _, p := range peers {
		if p.Host == "" {
			return fmt.Errorf("peers status: peer has no host")
		}
		r := (ref.Locator{Scheme: "peer", Target: p.Host}).String()
		name := p.Host
		if p.Alias != "" {
			name = p.Alias
		}
		attrs := map[string]any{
			"host": p.Host, "owned": p.Owned, "shared": p.Shared,
			"online": p.Online, "location": p.Location,
		}
		for k, v := range map[string]string{
			"alias": p.Alias, "version": p.Version, "commit": p.Commit,
			"os": p.OS, "arch": p.Arch, "os_version": p.OSVersion,
			"update_mode": p.UpdateMode,
		} {
			if v != "" {
				attrs[k] = v
			}
		}
		b.Node(provider.Node{Ref: r, Name: name, Kind: "peer", Type: "outpost/peer", Attrs: attrs})
		b.Edge(peersStatusSource, "contains", r, nil)
	}
	return b.Encode(w, "outpost", "op=peers-status")
}
