package mcpapi

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type clearClusterOut struct {
	OK             bool `json:"ok"`
	RestartPending bool `json:"restart_pending"`
}

// registerClusterTools — outpost_set_kubeconfig is gone; the
// bring-your-own paste path is no longer supported (outposts only
// join their owning cloudbox's cluster; for a different cluster,
// pair another outpost). outpost_clear_kubeconfig stays because
// "leave the cluster" remains a useful operator action.
func (s *Server) registerClusterTools() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_clear_kubeconfig",
		Description: "Clear the cluster credentials and the Enabled flag (i.e. leave the cluster). Joining happens via `outpost_set_builtins {cluster: true}` once paired; the daemon auto-fetches a kubeconfig from cloudbox on next boot.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, clearClusterOut, error) {
		res, err := s.core.ClearKubeconfig()
		if err != nil {
			return apiErrResult[clearClusterOut](err)
		}
		return nil, clearClusterOut{OK: res.OK, RestartPending: res.RestartPending}, nil
	})

	// outpost_cluster_leave / _join are the node-lifecycle pair, distinct from
	// clear_kubeconfig (a full wipe). leave DISABLES cluster mode but PRESERVES
	// this node's identity + mode so a rejoin comes back as the SAME kind of
	// node (agent stays agent), clearing only cloud-issued membership; join
	// re-enables it. The runtime container is reconciled on the ensuing restart:
	// leave's cluster-off boot tears it down, join's cluster-on boot recreates it
	// fresh (re-registering the kubelet). See docs/dks-node-model-and-venues.md.
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_cluster_leave",
		Description: "Leave the DKS cluster as this node: disable cluster mode, PRESERVING the node's mode + identity (agent stays agent), and clear only cloud-issued membership so a rejoin re-fetches fresh credentials. Pair with a restart to tear the runtime down.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, clearClusterOut, error) {
		res, err := s.core.LeaveCluster(ctx)
		if err != nil {
			return apiErrResult[clearClusterOut](err)
		}
		return nil, clearClusterOut{OK: res.OK, RestartPending: res.RestartPending}, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_cluster_join",
		Description: "Rejoin the DKS cluster as this node: re-enable cluster mode, retaining the preserved mode + identity. The boot reattach re-fetches fresh credentials; pair with a restart to bring the runtime up fresh.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, clearClusterOut, error) {
		res, err := s.core.JoinCluster()
		if err != nil {
			return apiErrResult[clearClusterOut](err)
		}
		return nil, clearClusterOut{OK: res.OK, RestartPending: res.RestartPending}, nil
	})
}
