// Package peerimage is the PEER half of the DKS "recipes, not blobs" image
// distribution model (docs/peer-dks-image-distribution.md). It is the peer-plane
// sibling of internal/agent/recipebuilder, which is the cloudbox-driven half.
//
// The model, unchanged from the design: a RECIPE (an indexed Dockerfile +
// build context + its sha256) travels between nodes; an image BLOB never does.
// Base images come from a public registry/CDN; app images are built locally on
// every node from the recipe. This package adds four verbs on top of that:
//
//	publish       persist a recipe locally + serve it to peers over the mesh
//	mesh-resolve  find the DISTINCT peers exposing the recipe service
//	ensure        make the recipe's image resident on THIS node, and prove it
//	report        emit identity-bound evidence that this node is in that state
//
// Four invariants shape every function here:
//
//  1. No cloudbox on the peer execution path. The transport is the existing
//     mesh forwarder (an allowlisted loopback service), never a new overlay.
//  2. Absence of evidence is never success. An unreachable node, an empty
//     listing, an unreadable digest and a missing report are each a FAILURE
//     with its own message — none of them collapses into "already satisfied".
//  3. What is resident is decided by the containerd CONTENT DIGEST, never by a
//     podman/ctr reference, which can be stale, retagged, or point at other
//     bytes. A digest that disagrees with the recorded provenance fails loudly
//     and is never "repaired" by fetching something else.
//  4. Evidence is bound to one identity. A report carries the (node, ref,
//     recipe, nonce) tuple it was challenged with; anything else — a node that
//     was never asked, a nonce belonging to another node, a second report from
//     the same node — is refused. See inspect.go.
package peerimage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// DigestState is the tri-state answer to "what is actually resident under this
// ref?". The three values are deliberately distinct: "I could not determine the
// digest" is NOT "the image is absent", and neither one is a pass.
type DigestState string

const (
	// StateResident — the ref exists and a well-formed content digest was read.
	StateResident DigestState = "resident"
	// StateAbsent — the runtime answered, and the ref is not in its image store.
	StateAbsent DigestState = "absent"
	// StateUnknown — the ref exists but its content digest could not be
	// determined. Never treat this as either resident or absent.
	StateUnknown DigestState = "unknown"
)

// Provenance is what THIS node recorded when it built and loaded a ref. It is
// the local anchor the live containerd digest is correlated against: the ref is
// mutable (a retag points it at different bytes without changing its name), the
// content digest is not.
type Provenance struct {
	// Node is the cluster node name this provenance belongs to — the node
	// NAME, never the outpost.dhnt.io/host label, which names a HOST and is
	// shared by every virtual backend that host runs.
	Node string `json:"node"`
	// Ref is the image reference the recipe builds to.
	Ref string `json:"ref"`
	// Recipe is the recipe name.
	Recipe string `json:"recipe"`
	// RecipeDigest is sha256:<hex> over the recipe's canonical form — the
	// CROSS-NODE identity. Two nodes running the same recipe agree here.
	RecipeDigest string `json:"recipe_digest"`
	// ContentDigest is the containerd content digest observed immediately
	// after the local build+load — the PER-NODE identity. Container builds
	// are not bit-reproducible, so two nodes running the same recipe are
	// expected to differ here; that is why the cross-node claim is made on
	// RecipeDigest and the local integrity claim on ContentDigest.
	ContentDigest string `json:"content_digest"`
	// BuiltAt is when the build+load completed.
	BuiltAt time.Time `json:"built_at"`
}

// Errors surfaced by this package. Callers map them onto their protocol's
// status; the shell harness mirrors them by name.
var (
	// ErrNoRecipe — the recipe is not in the local store. An unknown recipe is
	// a failure, never "nothing to do".
	ErrNoRecipe = errors.New("recipe not found")
	// ErrDigestMismatch — the live containerd digest disagrees with the
	// recorded provenance. Loud by design; never auto-repaired.
	ErrDigestMismatch = errors.New("resident content digest does not match recorded provenance")
	// ErrDigestUnknown — the ref exists but its content digest is unreadable.
	ErrDigestUnknown = errors.New("could not determine the resident content digest")
	// ErrNotResident — the runtime answered and the ref is absent.
	ErrNotResident = errors.New("image is not present in the node's containerd")
	// ErrNoProvenance — the ref is resident but this node has no record of
	// building it, so its bytes cannot be attributed to any recipe.
	ErrNoProvenance = errors.New("no local provenance for the resident image")
	// ErrNoPeers — nothing exposes the recipe service. An empty listing is a
	// failure, not a satisfied precondition.
	ErrNoPeers = errors.New("no peer exposes the recipe service")
	// ErrNotDistinct — fewer DISTINCT peers/nodes than the operation claims.
	ErrNotDistinct = errors.New("not enough distinct peers")
)

// Runtime reads what is actually resident in a node's containerd.
//
// Implementations MUST distinguish the three DigestStates and MUST return a
// non-nil error (rather than StateAbsent) when the runtime could not be
// consulted at all — an unreachable runtime is not an absent image.
type Runtime interface {
	ResidentDigest(ctx context.Context, ref string) (DigestState, string, error)
}

// Builder materializes a recipe into the node's containerd: resolve context →
// native build → load. It is an interface so Ensure is testable with neither
// podman nor a cluster.
type Builder interface {
	Materialize(ctx context.Context, recipeBody string) error
}

// NodeIdentity is how this daemon names itself in evidence. Name is the cluster
// NODE name (which carries the per-backend discriminator), not the host name:
// one host running two virtual backends is TWO nodes, and evidence that cannot
// tell them apart cannot prove it reached two.
type NodeIdentity struct {
	Name string
}

func (n NodeIdentity) validate() error {
	if strings.TrimSpace(n.Name) == "" {
		return fmt.Errorf("node name is required (evidence must name the node it came from)")
	}
	return nil
}
