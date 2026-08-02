package admincore

import (
	"context"
	"errors"

	"github.com/qiangli/outpost/internal/agent/peerimage"
)

// This file is the ONE business-logic definition of the four peer-image verbs.
// The MCP tools (mcpapi/tools_peerimage.go) and the CLI subcommands
// (cmd/outpost/peerimage.go) both dispatch here and nowhere else; the file key
// (conf.PeerImageConfig) is the fourth surface. PeerImageToolNames below is the
// single table those surfaces are checked against — see the parity tests.
//
// Side-effect class per verb (docs/settings.md carries the same table):
//
//	publish       Live  — writes the recipe store; nothing is read at boot.
//	mesh-resolve  Live  — read-only registry lookup.
//	ensure        Live  — builds + loads into this node's containerd.
//	report        Live  — read-only; reads containerd + local provenance.
//
// None of them restarts the daemon. The boot-time half (whether the recipe
// index is served at all, and on which mesh service name) lives in
// FileConfig.PeerImage and IS Boot-only — changing it restarts, which is why it
// is a builtins-style setting and not one of these verbs.

// PeerImageVerb names one of the four operations.
type PeerImageVerb string

const (
	PeerImageVerbPublish     PeerImageVerb = "publish"
	PeerImageVerbMeshResolve PeerImageVerb = "mesh-resolve"
	PeerImageVerbEnsure      PeerImageVerb = "ensure"
	PeerImageVerbReport      PeerImageVerb = "report"
)

// PeerImageToolNames maps each verb to its MCP tool name. MCP tool names are
// verb-noun; the CLI subcommand is the kebab-case verb under `outpost
// peer-image`. Both surfaces read this table rather than hard-coding strings,
// so a rename cannot desynchronize them.
var PeerImageToolNames = map[PeerImageVerb]string{
	PeerImageVerbPublish:     "outpost_publish_image_recipe",
	PeerImageVerbMeshResolve: "outpost_mesh_resolve_image_recipes",
	PeerImageVerbEnsure:      "outpost_ensure_image",
	PeerImageVerbReport:      "outpost_report_image",
}

// PeerImageVerbs is the canonical ordering used by the parity tests and the
// CLI help.
var PeerImageVerbs = []PeerImageVerb{
	PeerImageVerbPublish, PeerImageVerbMeshResolve, PeerImageVerbEnsure, PeerImageVerbReport,
}

// PeerImageOps is the daemon-side engine admincore drives. main.go wires in a
// *peerimage.Service; admincore keeps the nil-check, the validation and the
// error mapping so every surface gets identical behaviour.
type PeerImageOps interface {
	Publish(ctx context.Context, name, body string) (peerimage.Publication, error)
	Publications() ([]peerimage.Publication, error)
	MeshResolve(ctx context.Context, service string, minimum int) (peerimage.ResolveResult, error)
	Ensure(ctx context.Context, name string) (peerimage.EnsureResult, error)
	Report(ctx context.Context, ch peerimage.Challenge) (peerimage.Report, error)
}

const peerImageOffMsg = "peer image distribution is not enabled " +
	"(set peer_image.enabled and restart; the host must be paired with the mesh on)"

func (s *Server) peerImage() (PeerImageOps, error) {
	if s.deps.PeerImage == nil {
		return nil, badRequest("%s", peerImageOffMsg)
	}
	return s.deps.PeerImage, nil
}

// peerImageErr maps the engine's sentinel errors onto transport statuses. The
// message is passed through unchanged — it never contains a credential, and
// each sentinel is a distinct FAILURE (never a pass), which is the property the
// evidence invariant depends on.
func peerImageErr(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, peerimage.ErrNoRecipe):
		return notFound("%s", err.Error())
	case errors.Is(err, peerimage.ErrNoPeers), errors.Is(err, peerimage.ErrNotDistinct):
		return unavailable("%s", err.Error())
	case errors.Is(err, peerimage.ErrDigestMismatch):
		// Loud and terminal. A mismatch is never repaired by fetching other
		// bytes, so it is a conflict the operator must resolve, not a retry.
		return conflict("%s", err.Error())
	case errors.Is(err, peerimage.ErrDigestUnknown),
		errors.Is(err, peerimage.ErrNotResident),
		errors.Is(err, peerimage.ErrNoProvenance):
		return unavailable("%s", err.Error())
	}
	if ae := AsAPIError(err); ae != nil {
		return err
	}
	return badRequest("%s", err.Error())
}

// PeerImagePublish publishes a build recipe so peers can fetch and build it
// themselves. No image blob is transferred — that is the model.
//
// Side-effect class: Live.
func (s *Server) PeerImagePublish(ctx context.Context, name, body string) (peerimage.Publication, error) {
	ops, err := s.peerImage()
	if err != nil {
		return peerimage.Publication{}, err
	}
	if body == "" {
		return peerimage.Publication{}, badRequest("recipe body is required")
	}
	pub, err := ops.Publish(ctx, name, body)
	if err != nil {
		return peerimage.Publication{}, peerImageErr(err)
	}
	return pub, nil
}

// PeerImagePublications lists what this node publishes.
func (s *Server) PeerImagePublications() ([]peerimage.Publication, error) {
	ops, err := s.peerImage()
	if err != nil {
		return nil, err
	}
	pubs, err := ops.Publications()
	if err != nil {
		return nil, peerImageErr(err)
	}
	return pubs, nil
}

// PeerImageMeshResolve finds the DISTINCT peers exposing the recipe service.
//
// minimum is the number of distinct peers the caller intends to claim it
// reached; falling short is an error, and an empty registry answer is an error
// too. Neither is reported as an empty success.
//
// Side-effect class: Live (read-only).
func (s *Server) PeerImageMeshResolve(ctx context.Context, service string, minimum int) (peerimage.ResolveResult, error) {
	ops, err := s.peerImage()
	if err != nil {
		return peerimage.ResolveResult{}, err
	}
	if service == "" {
		return peerimage.ResolveResult{}, badRequest("service is required")
	}
	res, err := ops.MeshResolve(ctx, service, minimum)
	if err != nil {
		return res, peerImageErr(err)
	}
	return res, nil
}

// PeerImageEnsure makes a published recipe's image resident on THIS node and
// confirms it by containerd content digest. A digest that disagrees with the
// recorded provenance fails loudly and is never repaired by pulling anything.
//
// Side-effect class: Live.
func (s *Server) PeerImageEnsure(ctx context.Context, name string) (peerimage.EnsureResult, error) {
	ops, err := s.peerImage()
	if err != nil {
		return peerimage.EnsureResult{}, err
	}
	if name == "" {
		return peerimage.EnsureResult{}, badRequest("recipe name is required")
	}
	res, err := ops.Ensure(ctx, name)
	if err != nil {
		return res, peerImageErr(err)
	}
	return res, nil
}

// PeerImageReport answers an inspector's identity-bound challenge with this
// node's evidence. The reply always names THIS node; a challenge addressed to
// another node is refused rather than answered on its behalf.
//
// Side-effect class: Live (read-only).
func (s *Server) PeerImageReport(ctx context.Context, ch peerimage.Challenge) (peerimage.Report, error) {
	ops, err := s.peerImage()
	if err != nil {
		return peerimage.Report{}, err
	}
	if ch.Ref == "" {
		return peerimage.Report{}, badRequest("ref is required")
	}
	if ch.Nonce == "" {
		return peerimage.Report{}, badRequest("nonce is required; unbound evidence cannot be attributed to a node")
	}
	if !peerimage.ValidRecipeDigest(ch.Recipe) {
		return peerimage.Report{}, badRequest("recipe_digest must be sha256:<64 hex>")
	}
	rep, err := ops.Report(ctx, ch)
	if err != nil {
		return rep, peerImageErr(err)
	}
	return rep, nil
}
