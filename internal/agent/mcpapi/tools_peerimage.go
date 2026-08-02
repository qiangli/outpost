package mcpapi

import (
	"context"

	mcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qiangli/outpost/internal/agent/admincore"
	"github.com/qiangli/outpost/internal/agent/peerimage"
)

// Peer image distribution ("recipes, not blobs") — the four verbs. Every tool
// here is a thin wrapper over the ONE admincore method for its verb; the CLI
// subcommands (cmd/outpost/peerimage.go) and the file key (conf.PeerImageConfig)
// reach the same methods, which is what the parity tests prove.
//
// Side-effect classes (docs/settings.md carries the same table):
//
//	outpost_publish_image_recipe        Live  — writes the local recipe store.
//	outpost_mesh_resolve_image_recipes  Live  — read-only registry lookup.
//	outpost_ensure_image                Live  — builds + loads into containerd.
//	outpost_report_image                Live  — read-only digest + provenance.
//
// None restarts the daemon. The boot-time half — whether the recipe index is
// served at all, and under which mesh service name — is the peer_image file
// key, which IS Boot-only (a change restarts).

type peerImagePublishIn struct {
	Name string `json:"name,omitempty" jsonschema:"Recipe name. Optional when the recipe document names itself; must match when both are given."`
	Body string `json:"body" jsonschema:"The recipe document (flat ImageRecipe YAML). Only the recipe travels — never an image blob."`
}

type peerImagePublishOut struct {
	Publication peerimage.Publication `json:"publication"`
}

type peerImageListOut struct {
	Recipes []peerimage.Publication `json:"recipes"`
}

type peerImageMeshResolveIn struct {
	Service string `json:"service,omitempty" jsonschema:"Mesh service name to resolve (default: this node's configured peer_image.service)."`
	Minimum int    `json:"minimum,omitempty" jsonschema:"Minimum number of DISTINCT peers required (default 1). Falling short is an error, never an empty success."`
}

type peerImageMeshResolveOut struct {
	Result peerimage.ResolveResult `json:"result"`
}

type peerImageEnsureIn struct {
	Name string `json:"name" jsonschema:"Name of a recipe in the local store (publish it or fetch it from a peer first)."`
}

type peerImageEnsureOut struct {
	Result peerimage.EnsureResult `json:"result"`
}

type peerImageReportIn struct {
	Node         string `json:"node,omitempty" jsonschema:"Node the challenge addresses. Answered only when it names THIS node (or is empty); a challenge for another node is refused, never answered on its behalf."`
	Ref          string `json:"ref" jsonschema:"Image reference the evidence is about."`
	RecipeDigest string `json:"recipe_digest" jsonschema:"The recipe's cross-node identity, sha256:<64 hex>."`
	Nonce        string `json:"nonce" jsonschema:"The per-(node,ref,recipe) challenge nonce. Unbound evidence cannot be attributed, so it is required."`
}

type peerImageReportOut struct {
	Report peerimage.Report `json:"report"`
}

func (s *Server) registerPeerImageTools() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        admincore.PeerImageToolNames[admincore.PeerImageVerbPublish],
		Description: "Publish a build recipe so peers can fetch and build it themselves (recipes, not blobs — no image bytes move). Live.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in peerImagePublishIn) (*mcp.CallToolResult, peerImagePublishOut, error) {
		pub, err := s.core.PeerImagePublish(context.Background(), in.Name, in.Body)
		if err != nil {
			return apiErrResult[peerImagePublishOut](err)
		}
		return nil, peerImagePublishOut{Publication: pub}, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_list_image_recipes",
		Description: "List the recipes this node currently publishes to peers. Live (read-only).",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, peerImageListOut, error) {
		pubs, err := s.core.PeerImagePublications()
		if err != nil {
			return apiErrResult[peerImageListOut](err)
		}
		return nil, peerImageListOut{Recipes: pubs}, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        admincore.PeerImageToolNames[admincore.PeerImageVerbMeshResolve],
		Description: "Find the DISTINCT peers exposing the recipe service over the mesh. An empty registry answer is an error, not an empty success. Live (read-only).",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in peerImageMeshResolveIn) (*mcp.CallToolResult, peerImageMeshResolveOut, error) {
		res, err := s.core.PeerImageMeshResolve(context.Background(), in.Service, in.Minimum)
		if err != nil {
			return apiErrResult[peerImageMeshResolveOut](err)
		}
		return nil, peerImageMeshResolveOut{Result: res}, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        admincore.PeerImageToolNames[admincore.PeerImageVerbEnsure],
		Description: "Make a published recipe's image resident on THIS node, confirmed by containerd content digest. A digest that disagrees with the recorded provenance fails loudly and is never repaired by pulling anything. Live.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in peerImageEnsureIn) (*mcp.CallToolResult, peerImageEnsureOut, error) {
		res, err := s.core.PeerImageEnsure(context.Background(), in.Name)
		if err != nil {
			return apiErrResult[peerImageEnsureOut](err)
		}
		return nil, peerImageEnsureOut{Result: res}, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        admincore.PeerImageToolNames[admincore.PeerImageVerbReport],
		Description: "Answer an inspector's identity-bound challenge with this node's evidence (containerd digest + build provenance). The reply always names THIS node; a challenge addressed to another node is refused. Live (read-only).",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in peerImageReportIn) (*mcp.CallToolResult, peerImageReportOut, error) {
		rep, err := s.core.PeerImageReport(context.Background(), peerimage.Challenge{
			Node: in.Node, Ref: in.Ref, Recipe: in.RecipeDigest, Nonce: in.Nonce,
		})
		if err != nil {
			return apiErrResult[peerImageReportOut](err)
		}
		return nil, peerImageReportOut{Report: rep}, nil
	})
}
