package peerimage

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/qiangli/outpost/internal/agent/recipebuilder"
)

// PeerRef is one peer from the mesh service registry. It is deliberately the
// same shape admincore.MeshResolvedPeer carries, minus the import.
type PeerRef struct {
	Host     string   `json:"host"`
	PeerID   string   `json:"peer_id"`
	Services []string `json:"services,omitempty"`
}

// Service is the node-local peer-image engine. All four verbs run here; the
// admincore/MCP/CLI surfaces are thin wrappers over these four methods.
type Service struct {
	// Identity names THIS node in every piece of evidence it emits.
	Identity NodeIdentity
	// Store holds published recipes + this node's build provenance.
	Store *Store
	// Runtime reads what containerd actually has.
	Runtime Runtime
	// Build materializes a recipe locally. nil → Ensure can verify but not
	// build, and says so rather than reporting success.
	Build Builder
	// Resolver returns the peers exposing a mesh service. nil → mesh-resolve
	// reports the mesh is unavailable rather than returning an empty set.
	Resolver func(service string) ([]PeerRef, error)

	now func() time.Time
}

func (s *Service) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *Service) check() error {
	if s == nil {
		return fmt.Errorf("peer image distribution is not enabled on this host")
	}
	if err := s.Identity.validate(); err != nil {
		return err
	}
	if s.Store == nil {
		return fmt.Errorf("recipe store is not configured")
	}
	return nil
}

// ---------------------------------------------------------------------------
// verb: publish
// ---------------------------------------------------------------------------

// Publish stores a recipe locally and makes it fetchable by peers through the
// mesh recipe index. It transfers no image bytes — publishing a recipe is the
// whole point of the model.
//
// Side-effect class: Live.
func (s *Service) Publish(_ context.Context, name, body string) (Publication, error) {
	if err := s.check(); err != nil {
		return Publication{}, err
	}
	return s.Store.Publish(name, body)
}

// Publications lists what this node currently publishes.
func (s *Service) Publications() ([]Publication, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	return s.Store.List()
}

// ---------------------------------------------------------------------------
// verb: mesh-resolve
// ---------------------------------------------------------------------------

// ResolveResult is a mesh-resolve outcome.
type ResolveResult struct {
	Service string    `json:"service"`
	Peers   []PeerRef `json:"peers"`
	// Distinct is the number of DISTINCT peer identities, which is what any
	// "reaches N peers" claim must be made from.
	Distinct int `json:"distinct"`
}

// MeshResolve finds the peers exposing service, requiring at least `minimum`
// DISTINCT peer identities.
//
// An empty registry answer is ErrNoPeers, never an empty success: "nobody
// exposes it" and "we reached everybody who does (zero)" are not the same
// claim, and only the second one could be read as satisfied.
//
// Side-effect class: Live (read-only).
func (s *Service) MeshResolve(_ context.Context, service string, minimum int) (ResolveResult, error) {
	if strings.TrimSpace(service) == "" {
		return ResolveResult{}, fmt.Errorf("service is required")
	}
	if s == nil || s.Resolver == nil {
		return ResolveResult{}, fmt.Errorf("mesh service registry is not available (pair the host and enable the mesh)")
	}
	peers, err := s.Resolver(service)
	if err != nil {
		return ResolveResult{}, err
	}
	distinct, err := DistinctPeers(peers, minimum)
	if err != nil {
		return ResolveResult{Service: service}, fmt.Errorf("%w (service %q)", err, service)
	}
	return ResolveResult{Service: service, Peers: distinct, Distinct: len(distinct)}, nil
}

// DistinctPeers dedupes by peer id and enforces the minimum. Entries without a
// peer id are dropped: a peer we cannot dial is not a peer we reached.
func DistinctPeers(peers []PeerRef, minimum int) ([]PeerRef, error) {
	if minimum < 1 {
		minimum = 1
	}
	seen := make(map[string]struct{}, len(peers))
	out := make([]PeerRef, 0, len(peers))
	for _, p := range peers {
		id := strings.TrimSpace(p.PeerID)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, ErrNoPeers
	}
	if len(out) < minimum {
		return out, fmt.Errorf("%w: %d distinct peer(s), %d required", ErrNotDistinct, len(out), minimum)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// verb: ensure
// ---------------------------------------------------------------------------

// EnsureResult describes the state this node ended in.
type EnsureResult struct {
	Node          string      `json:"node"`
	Recipe        string      `json:"recipe"`
	Ref           string      `json:"ref"`
	RecipeDigest  string      `json:"recipe_digest"`
	State         DigestState `json:"state"`
	ContentDigest string      `json:"content_digest,omitempty"`
	// Built is true when this call performed the build; false means the image
	// was already resident AND its digest correlated with the provenance.
	Built bool `json:"built"`
}

// Ensure makes the named recipe's image resident on THIS node and proves it by
// content digest.
//
// The sequence, and why each step is where it is:
//
//  1. Load the recipe. Absent → ErrNoRecipe. There is no "nothing to do".
//  2. Read the live containerd digest.
//     resident + provenance agrees  → done, Built=false.
//     resident + provenance differs → ErrDigestMismatch, LOUD, no repair.
//     The ref was retagged onto other bytes;
//     fetching something else would hide it.
//     resident + no provenance      → rebuild from the verified recipe, which
//     is the only thing that lets this node
//     attribute the bytes it runs.
//     unknown                       → ErrDigestUnknown. Not absent, not a pass.
//     absent                        → build.
//  3. Build + load, then read the digest BACK. A build that "succeeded" without
//     producing a readable resident digest is a failure.
//  4. Record provenance only from the digest actually read in step 3.
//
// Side-effect class: Live.
func (s *Service) Ensure(ctx context.Context, name string) (EnsureResult, error) {
	if err := s.check(); err != nil {
		return EnsureResult{}, err
	}
	if s.Runtime == nil {
		return EnsureResult{}, fmt.Errorf("no container runtime is configured; cannot determine what is resident")
	}
	body, rec, err := s.Store.Recipe(name)
	if err != nil {
		return EnsureResult{}, err
	}
	res := EnsureResult{
		Node: s.Identity.Name, Recipe: name, Ref: rec.Ref(), RecipeDigest: rec.Digest(),
	}

	state, live, err := s.Runtime.ResidentDigest(ctx, rec.Ref())
	if err != nil {
		return res, err
	}
	prov, hasProv, err := s.Store.Provenance(rec.Ref())
	if err != nil {
		return res, err
	}

	switch state {
	case StateResident:
		if hasProv {
			if prov.ContentDigest != live {
				res.State, res.ContentDigest = state, live
				return res, fmt.Errorf("%w: ref %s is resident as %s but was built as %s",
					ErrDigestMismatch, rec.Ref(), live, prov.ContentDigest)
			}
			if prov.RecipeDigest == rec.Digest() {
				res.State, res.ContentDigest = state, live
				return res, nil // already satisfied, and proven so
			}
			// Same bytes, different recipe: the recipe moved on. Rebuild.
		}
	case StateUnknown:
		return res, fmt.Errorf("%w: ref %s", ErrDigestUnknown, rec.Ref())
	case StateAbsent:
		// fall through to build
	default:
		return res, fmt.Errorf("unknown digest state %q for ref %s", string(state), rec.Ref())
	}

	if s.Build == nil {
		return res, fmt.Errorf("no builder is configured; cannot materialize recipe %s", name)
	}
	if err := s.Build.Materialize(ctx, body); err != nil {
		return res, fmt.Errorf("build recipe %s: %w", name, err)
	}

	state, live, err = s.Runtime.ResidentDigest(ctx, rec.Ref())
	if err != nil {
		return res, err
	}
	res.State, res.ContentDigest, res.Built = state, live, true
	switch state {
	case StateResident:
	case StateAbsent:
		return res, fmt.Errorf("%w after build: %s", ErrNotResident, rec.Ref())
	default:
		return res, fmt.Errorf("%w after build: %s", ErrDigestUnknown, rec.Ref())
	}
	if !ValidContentDigest(live) {
		return res, fmt.Errorf("%w after build: %s", ErrDigestUnknown, rec.Ref())
	}
	if err := s.Store.PutProvenance(Provenance{
		Node: s.Identity.Name, Ref: rec.Ref(), Recipe: name,
		RecipeDigest: rec.Digest(), ContentDigest: live, BuiltAt: s.clock().UTC(),
	}); err != nil {
		return res, err
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// verb: report
// ---------------------------------------------------------------------------

// Report answers a Challenge with this node's identity-bound evidence.
//
// The node name in the answer is always THIS daemon's identity — never the
// challenge's — so a challenge addressed to another node produces a refusal
// rather than a report attributed to the wrong node.
//
// Side-effect class: Live (read-only).
func (s *Service) Report(ctx context.Context, ch Challenge) (Report, error) {
	if err := s.check(); err != nil {
		return Report{}, err
	}
	if strings.TrimSpace(ch.Node) != "" && ch.Node != s.Identity.Name {
		return Report{}, fmt.Errorf("challenge is addressed to node %q; this node is %q", ch.Node, s.Identity.Name)
	}
	if strings.TrimSpace(ch.Nonce) == "" {
		return Report{}, fmt.Errorf("challenge nonce is required; an unbound report cannot be attributed")
	}
	if s.Runtime == nil {
		return Report{}, fmt.Errorf("no container runtime is configured; cannot determine what is resident")
	}

	ref := ch.Ref
	recipeDigest := ch.Recipe
	if strings.TrimSpace(ref) == "" {
		return Report{}, fmt.Errorf("challenge ref is required")
	}
	if !ValidRecipeDigest(recipeDigest) {
		return Report{}, fmt.Errorf("challenge recipe digest must be sha256:<64 hex>")
	}

	// Identity is echoed exactly as challenged, with this node's own name.
	r := Report{Node: s.Identity.Name, Ref: ref, Recipe: recipeDigest, Nonce: ch.Nonce}

	state, live, err := s.Runtime.ResidentDigest(ctx, ref)
	if err != nil {
		// A runtime we could not consult is reported as unknown WITH the
		// reason. It is never reported as absent, and never as resident.
		r.State = StateUnknown
		r.Detail = "runtime could not be consulted"
		return r, err
	}
	r.State, r.ContentDigest = state, live
	if prov, ok, perr := s.Store.Provenance(ref); perr == nil && ok {
		r.ProvenanceDigest = prov.ContentDigest
		if prov.RecipeDigest != recipeDigest {
			// Say so plainly; the Inspector will refuse it on identity.
			r.Detail = "local provenance descends from a different recipe"
		}
	} else if perr != nil {
		r.Detail = "provenance unreadable"
	}
	return r, nil
}

// ---------------------------------------------------------------------------
// fetching a peer's recipe
// ---------------------------------------------------------------------------

// FetchRecipe pulls a recipe document from a peer's index through an already-
// open mesh forward at forwardAddr, and stores it locally under its own name.
//
// forwardAddr is loopback by construction (it is a listener this daemon
// opened), so it is passed to the fetcher as an EXACT allowance. Any redirect
// that leaves it — including one that lands on a different loopback port, e.g.
// the admin/MCP surface — is refused at that hop. See safehttp.go.
func (s *Service) FetchRecipe(ctx context.Context, forwardAddr, name string) (Publication, error) {
	if err := s.check(); err != nil {
		return Publication{}, err
	}
	if !nameRE.MatchString(name) {
		return Publication{}, fmt.Errorf("invalid recipe name")
	}
	if strings.TrimSpace(forwardAddr) == "" {
		return Publication{}, fmt.Errorf("mesh forward address is required")
	}
	f := NewFetcher(Allow(forwardAddr))
	body, err := f.Get(ctx, "http://"+forwardAddr+"/recipes/"+name)
	if err != nil {
		return Publication{}, err
	}
	if _, perr := recipebuilder.ParseRecipe(string(body)); perr != nil {
		return Publication{}, fmt.Errorf("peer served an invalid recipe: %w", perr)
	}
	return s.Store.Publish(name, string(body))
}
