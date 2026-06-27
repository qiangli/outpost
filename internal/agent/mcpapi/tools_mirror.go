package mcpapi

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qiangli/outpost/internal/agent/admincore"
)

func (s *Server) registerMirrorTools() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_mirror_set",
		Description: "Add/update a mobility-aware live directory mirror: keep a peer's replica in sync with local <source>, but ONLY while the peer exposing mesh service <service> is reachable (and same-LAN when lan_only). Pauses when the pair goes remote, resumes + catches up (full sync) when local again. Continuous live replica — distinct from the encrypted Backup snapshots. Triggers a restart.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in mirrorSetIn) (*mcp.CallToolResult, meshOKOut, error) {
		if err := s.core.MirrorUpsert(in.Source, in.Service, in.LANOnly); err != nil {
			return apiErrResult[meshOKOut](err)
		}
		return nil, meshOKOut{OK: true}, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_mirror_rm",
		Description: "Remove a live directory-mirror job by its source directory. Triggers a restart.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in mirrorSourceIn) (*mcp.CallToolResult, meshOKOut, error) {
		if err := s.core.MirrorDelete(in.Source); err != nil {
			return apiErrResult[meshOKOut](err)
		}
		return nil, meshOKOut{OK: true}, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_mirror_list",
		Description: "List the live directory-mirror jobs (source → mesh service, lan_only).",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, mirrorListOut, error) {
		v, err := s.core.Mirror()
		if err != nil {
			return apiErrResult[mirrorListOut](err)
		}
		return nil, mirrorListOut{Enabled: v.Enabled, Jobs: v.Jobs}, nil
	})
}

type mirrorSetIn struct {
	Source  string `json:"source" jsonschema:"Local directory to mirror"`
	Service string `json:"service" jsonschema:"Mesh service name the replica peer exposes to receive the mirror (e.g. an 'rclone serve webdav' advertised as 'webdav')"`
	LANOnly bool   `json:"lan_only,omitempty" jsonschema:"Mirror only while the peer is on the same LAN (pause over WAN)"`
}

type mirrorSourceIn struct {
	Source string `json:"source" jsonschema:"Source directory of the mirror job to remove"`
}

type mirrorListOut struct {
	Enabled bool                      `json:"enabled"`
	Jobs    []admincore.MirrorJobView `json:"jobs"`
}
