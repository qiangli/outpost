package mcpapi

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qiangli/outpost/internal/agent/admincore"
)

type federationOnIn struct {
	ClusterName string `json:"cluster_name,omitempty" jsonschema:"name this host's plane registers under; defaults to the agent name"`
	APIPort     int    `json:"api_port,omitempty" jsonschema:"loopback port of the federation apiserver forward; defaults to 15443"`
	Kubeconfig  string `json:"kubeconfig,omitempty" jsonschema:"override path for the materialised Karmada kubeconfig"`
}

type federationOffIn struct {
	Force bool `json:"force,omitempty" jsonschema:"clear the config even if the karmada built-in cannot be uninstalled"`
}

// registerFederationTools exposes Karmada federation. Reveal is its own tool so
// reading federation status never pulls a join credential into a transcript.
func (s *Server) registerFederationTools() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_federation",
		Description: "Report Karmada federation state for this host: role, install phase, CA hash / federation id, whether the host's own plane is registered. Never returns credentials — use outpost_federation_token for those.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, admincore.FederationResult, error) {
		res, err := s.core.FederationStatus()
		if err != nil {
			return apiErrResult[admincore.FederationResult](err)
		}
		return nil, res, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_federation_on",
		Description: "Make this host a Karmada federation host: publish the Karmada apiserver over the tunnel, install the karmada built-in, and register this host's own plane. Requires a hosted control plane. Triggers a restart (the control-plane container is recreated; data is kept). Not supported on Windows.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in federationOnIn) (*mcp.CallToolResult, admincore.FederationResult, error) {
		res, err := s.core.FederationOn(admincore.FederationParams{ClusterName: in.ClusterName, APIPort: in.APIPort, Kubeconfig: in.Kubeconfig})
		if err != nil {
			return apiErrResult[admincore.FederationResult](err)
		}
		return nil, res, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_federation_off",
		Description: "Stop federating: uninstall the karmada built-in, remove the materialised kubeconfig and clear the federation config. Members registered elsewhere are not touched. Triggers a restart.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in federationOffIn) (*mcp.CallToolResult, admincore.FederationResult, error) {
		res, err := s.core.FederationOff(ctx, admincore.FederationParams{Force: in.Force})
		if err != nil {
			return apiErrResult[admincore.FederationResult](err)
		}
		return nil, res, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_federation_token",
		Description: "Mint a Karmada bootstrap token and reveal what a member needs to join: tunnel endpoint, tunnel token, STCP secret, bootstrap token, CA hash. These are credentials — treat them like the control-plane token. The join_hint names env vars, never literals.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, admincore.FederationTokenResult, error) {
		res, err := s.core.FederationToken(ctx)
		if err != nil {
			return apiErrResult[admincore.FederationTokenResult](err)
		}
		return nil, res, nil
	})
}
