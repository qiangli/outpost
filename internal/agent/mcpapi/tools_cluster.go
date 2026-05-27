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
}
