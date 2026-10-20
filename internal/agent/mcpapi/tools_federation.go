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
	Force         bool   `json:"force,omitempty" jsonschema:"clear the config even if the karmada built-in cannot be uninstalled"`
	KarmadaConfig string `json:"karmada_config,omitempty" jsonschema:"leave: host-accessible Karmada admin kubeconfig path for control-plane RBAC cleanup"`
}

type federationJoinIn struct {
	Endpoint       string `json:"endpoint" jsonschema:"the federation host's frps address, host:port (port defaults to 7000)"`
	Token          string `json:"token" jsonschema:"the federation host's tunnel token"`
	STCPSecret     string `json:"stcp_secret" jsonschema:"the federation host's STCP secret"`
	BootstrapToken string `json:"bootstrap_token" jsonschema:"Karmada bootstrap token from outpost_federation_token (24h, single use)"`
	CAHash         string `json:"ca_hash" jsonschema:"Karmada CA discovery hash, sha256:<hex>"`
	ClusterName    string `json:"cluster_name,omitempty" jsonschema:"name this plane registers under; defaults to the agent name"`
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
		res, err := s.core.FederationOff(ctx, admincore.FederationParams{Force: in.Force, KarmadaConfig: in.KarmadaConfig})
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

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_federation_join",
		Description: "Join this host's DKS plane to a Karmada federation as a pull-mode member, with the values outpost_federation_token printed on the federation host. Requires a hosted control plane. Stores everything except the bootstrap token in the config (the token sits in a 0600 pending-join file until registration succeeds) and triggers a restart; the daemon then brings up the federation visitor, mirrors the Karmada apiserver Service and runs karmadactl register. Follow progress with outpost_federation. Not supported on Windows.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in federationJoinIn) (*mcp.CallToolResult, admincore.FederationResult, error) {
		res, err := s.core.FederationJoin(admincore.FederationJoinParams{
			Endpoint: in.Endpoint, Token: in.Token, STCPSecret: in.STCPSecret,
			BootstrapToken: in.BootstrapToken, CAHash: in.CAHash, ClusterName: in.ClusterName,
		})
		if err != nil {
			return apiErrResult[admincore.FederationResult](err)
		}
		return nil, res, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_federation_leave",
		Description: "Leave the Karmada federation this member joined: karmadactl unregister (removes karmada-agent and the Cluster object), remove the mirror Service/Endpoints and the federation visitor. Triggers a restart.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in federationOffIn) (*mcp.CallToolResult, admincore.FederationResult, error) {
		res, err := s.core.FederationLeave(ctx, admincore.FederationParams{Force: in.Force, KarmadaConfig: in.KarmadaConfig})
		if err != nil {
			return apiErrResult[admincore.FederationResult](err)
		}
		return nil, res, nil
	})
}
