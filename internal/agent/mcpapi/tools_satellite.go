package mcpapi

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qiangli/outpost/internal/agent/admincore"
)

type satelliteAttachIn struct {
	Satellite string `json:"satellite" jsonschema:"Satellite id (or a unique name) from outpost_satellite_list"`
	Migrate   bool   `json:"migrate,omitempty" jsonschema:"Replace a legacy top-level tunnel pairing with the attachment"`
	DryRun    bool   `json:"dry_run,omitempty" jsonschema:"Return the requests that would be sent; send nothing"`
}

type satelliteDetachIn struct {
	Satellite string `json:"satellite,omitempty" jsonschema:"Satellite id or name; empty = the only attachment"`
	DryRun    bool   `json:"dry_run,omitempty" jsonschema:"Return the requests that would be sent; send nothing"`
}

func (s *Server) registerSatelliteTools() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_satellite_list",
		Description: "List the satellites this host may attach to (its owner's, plus those of mutual members), each with attached (per the master) and local (credential held in agent.json). Needs the cloudbox pairing.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, admincore.SatelliteListResult, error) {
		res, err := s.core.SatelliteList(ctx)
		if err != nil {
			return apiErrResult[admincore.SatelliteListResult](err)
		}
		return nil, res, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_satellite_attach",
		Description: "Attach this host to one satellite with its cloudbox pairing (no pairing code): master attach token -> satellite host token + tunnel registration, stored as attachments[0]. The daemon restarts and tunnels there, publishing the local apps. A second satellite while attached fails with 409 attached_elsewhere.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in satelliteAttachIn) (*mcp.CallToolResult, admincore.SatelliteResult, error) {
		res, err := s.core.SatelliteAttach(ctx, admincore.SatelliteAttachParams{Satellite: in.Satellite, Migrate: in.Migrate, DryRun: in.DryRun})
		if err != nil {
			return apiErrResult[admincore.SatelliteResult](err)
		}
		return nil, res, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_satellite_detach",
		Description: "Detach from a satellite: leave it (DELETE /api/v1/hosts/self there), remove the assignment at the master, drop the attachment and restart to stop the tunnel.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in satelliteDetachIn) (*mcp.CallToolResult, admincore.SatelliteResult, error) {
		res, err := s.core.SatelliteDetach(ctx, admincore.SatelliteDetachParams{Satellite: in.Satellite, DryRun: in.DryRun})
		if err != nil {
			return apiErrResult[admincore.SatelliteResult](err)
		}
		return nil, res, nil
	})
}
