package admincore

import (
	"context"

	"github.com/qiangli/outpost/internal/agent/peerstatus"
)

// PeerStatus queries cloudbox for the peer status board — online state, a
// same-LAN/remote location hint, and the build/OS/arch details each host
// last reported — for the paired hosts this account can see (its owned
// hosts plus hosts shared with it). Requires a paired host (CloudboxBase
// + access token are set). Backs the outpost_peers_status MCP tool; the
// `outpost peers status` CLI calls peerstatus.Fetch directly so it works
// without the daemon running.
//
// Cloudbox computes each peer's location by comparing the host's
// last-recorded egress IP to the caller's source IP — a heuristic that
// false-negatives (reports "remote" for hosts that ARE on the same LAN
// when the recorded egress IPs differ). When this daemon's mesh data
// plane holds a DIRECT (non-relayed) link to a peer, its link class is
// the ground truth, so we override the cloudbox hint with it.
func (s *Server) PeerStatus(ctx context.Context) ([]peerstatus.Peer, error) {
	peers, err := peerstatus.Fetch(ctx, s.deps.CloudboxBase, s.deps.CloudboxAccessToken, nil)
	if err != nil {
		return nil, err
	}
	if s.deps.MeshLinkClassByHost != nil {
		for i := range peers {
			peers[i].Location = overrideLocation(
				peers[i].Location, s.deps.MeshLinkClassByHost(peers[i].Host))
		}
	}
	return peers, nil
}

// overrideLocation corrects cloudbox's egress-IP location heuristic with the
// mesh link class when a direct link exists. tp/lan (link-local / RFC-1918)
// are definitively same-LAN; wan (public remote addr) is definitively remote;
// "" (no direct link / relay-only) falls back to whatever cloudbox computed.
func overrideLocation(cloudboxLoc, linkClass string) string {
	switch linkClass {
	case "tp", "lan":
		return "same_lan"
	case "wan":
		return "remote"
	default: // "" — no direct mesh link; trust the cloudbox heuristic
		return cloudboxLoc
	}
}
