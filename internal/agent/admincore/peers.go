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
func (s *Server) PeerStatus(ctx context.Context) ([]peerstatus.Peer, error) {
	return peerstatus.Fetch(ctx, s.deps.CloudboxBase, s.deps.CloudboxAccessToken, nil)
}
