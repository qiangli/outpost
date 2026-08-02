package peerimage

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// Challenge is one node's identity-bound ask. All four fields travel together
// and a Report must echo all four: the node it is about, the ref it is about,
// the recipe digest it is expected to descend from, and a nonce that exists
// only for this (node, ref, recipe) triple.
//
// The nonce is what makes evidence non-transferable. Without it, a report is
// just a claim about a node name, and one node's answer can be replayed as
// another's — which is exactly how an "N nodes reached" result gets fabricated
// from one node.
type Challenge struct {
	Node   string `json:"node"`
	Ref    string `json:"ref"`
	Recipe string `json:"recipe_digest"`
	Nonce  string `json:"nonce"`
}

// Report is one node's answer to its Challenge. The first four fields are the
// echoed identity; the rest is the observation.
type Report struct {
	Node   string `json:"node"`
	Ref    string `json:"ref"`
	Recipe string `json:"recipe_digest"`
	Nonce  string `json:"nonce"`

	// State is the tri-state containerd answer.
	State DigestState `json:"state"`
	// ContentDigest is what containerd says is resident RIGHT NOW.
	ContentDigest string `json:"content_digest,omitempty"`
	// ProvenanceDigest is what this node recorded when it built the ref.
	// Empty means the node cannot attribute the resident bytes to any recipe.
	ProvenanceDigest string `json:"provenance_digest,omitempty"`
	// Detail is a human note; never authoritative, never trusted.
	Detail string `json:"detail,omitempty"`
}

// Verdict is the per-node outcome the Inspector reached.
type Verdict struct {
	Node          string `json:"node"`
	OK            bool   `json:"ok"`
	Reason        string `json:"reason,omitempty"`
	ContentDigest string `json:"content_digest,omitempty"`
}

// Summary is the whole inspection's outcome.
type Summary struct {
	// Ref + RecipeDigest are the cross-node claim being proven.
	Ref          string `json:"ref"`
	RecipeDigest string `json:"recipe_digest"`
	// Asked is how many DISTINCT nodes were challenged.
	Asked int `json:"asked"`
	// Proven is how many DISTINCT nodes returned accepted evidence.
	Proven int `json:"proven"`
	// OK is true only when Proven == Asked and Asked >= the required minimum.
	OK       bool      `json:"ok"`
	Reason   string    `json:"reason,omitempty"`
	Verdicts []Verdict `json:"verdicts"`
	// Rejected records evidence the Inspector refused, with the rule that
	// refused it. Rejections are surfaced, never silently dropped.
	Rejected []Verdict `json:"rejected,omitempty"`
}

// Inspector collects identity-bound evidence from a fixed, DISTINCT set of
// nodes and decides whether the claim holds. It is a pure state machine — no
// I/O, no clock, no randomness once constructed — so both the Go callers and
// the shell harness can be tested to the same rules offline.
type Inspector struct {
	ref     string
	recipe  string
	minimum int

	asked map[string]Challenge // node → its challenge

	seenNode  map[string]struct{} // nodes that already reported
	seenNonce map[string]string   // nonce → the node it was issued to

	verdicts []Verdict
	rejected []Verdict
}

// NonceFunc mints a fresh per-node nonce. Injected so tests are deterministic.
type NonceFunc func() (string, error)

// RandomNonce is the production NonceFunc: 128 bits of crypto/rand, hex.
func RandomNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// NewInspector builds an Inspector for ref@recipeDigest over nodes.
//
// It REFUSES to start unless the node set is genuinely distinct and at least
// `minimum` large. This is the requirement-2 gate: an operation that claims to
// reach N nodes must be constructed from N distinct node NAMES up front, so a
// duplicated node cannot inflate the count later. Node names carry the
// per-backend discriminator; the outpost.dhnt.io/host LABEL does not (two
// virtual backends on one host share it), so it is never an identity here.
func NewInspector(ref, recipeDigest string, nodes []string, minimum int, nonce NonceFunc) (*Inspector, error) {
	if strings.TrimSpace(ref) == "" {
		return nil, fmt.Errorf("ref is required")
	}
	if !ValidRecipeDigest(recipeDigest) {
		return nil, fmt.Errorf("recipe digest must be sha256:<64 hex>")
	}
	if minimum < 1 {
		minimum = 1
	}
	if nonce == nil {
		nonce = RandomNonce
	}
	asked := make(map[string]Challenge, len(nodes))
	for _, n := range nodes {
		n = strings.TrimSpace(n)
		if n == "" {
			return nil, fmt.Errorf("%w: an unnamed node cannot be identified", ErrNotDistinct)
		}
		if _, dup := asked[n]; dup {
			return nil, fmt.Errorf("%w: node %q listed twice", ErrNotDistinct, n)
		}
		nc, err := nonce()
		if err != nil {
			return nil, fmt.Errorf("mint nonce for %s: %w", n, err)
		}
		asked[n] = Challenge{Node: n, Ref: ref, Recipe: recipeDigest, Nonce: nc}
	}
	if len(asked) < minimum {
		return nil, fmt.Errorf("%w: %d distinct node(s) available, %d required", ErrNotDistinct, len(asked), minimum)
	}
	return &Inspector{
		ref: ref, recipe: recipeDigest, minimum: minimum,
		asked:     asked,
		seenNode:  map[string]struct{}{},
		seenNonce: map[string]string{},
	}, nil
}

// Challenges returns the per-node asks, sorted by node name for determinism.
func (i *Inspector) Challenges() []Challenge {
	out := make([]Challenge, 0, len(i.asked))
	for _, c := range i.asked {
		out = append(out, c)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Node < out[b].Node })
	return out
}

// Accept admits one report, or refuses it with the rule that refused it.
//
// The order of the checks matters: identity is settled BEFORE the observation
// is read, so a forged or replayed report is never scored on its contents.
func (i *Inspector) Accept(r Report) error {
	node := strings.TrimSpace(r.Node)

	// (1) Foreign node — evidence from a node that was never asked. This is
	// the check that stops a third party's answer from padding the count.
	ch, asked := i.asked[node]
	if node == "" || !asked {
		return i.reject(node, "foreign node: %q was never challenged", r.Node)
	}

	// (2) Foreign / replayed evidence — a nonce belonging to a different node,
	// or one already spent. Checked before the node-duplicate rule so a replay
	// is named as a replay rather than as a repeat.
	if owner, spent := i.seenNonce[r.Nonce]; spent {
		return i.reject(node, "duplicate evidence: nonce already presented by node %q", owner)
	}
	if r.Nonce == "" || r.Nonce != ch.Nonce {
		return i.reject(node, "foreign evidence: nonce was not issued to node %q", node)
	}

	// (3) Duplicate node — a second report from an already-answered node.
	if _, dup := i.seenNode[node]; dup {
		return i.reject(node, "duplicate evidence: node %q already reported", node)
	}

	// (4) Identity binding — the report must be about the ref and recipe the
	// node was actually challenged with, so it cannot be attributed to the
	// wrong image or the wrong build.
	if r.Ref != ch.Ref {
		return i.reject(node, "identity mismatch: report is about ref %q, challenge was %q", r.Ref, ch.Ref)
	}
	if r.Recipe != ch.Recipe {
		return i.reject(node, "identity mismatch: report cites recipe %q, challenge was %q", r.Recipe, ch.Recipe)
	}

	// The nonce is spent the moment the identity clears, so a report that is
	// then rejected on its CONTENTS still cannot be resubmitted.
	i.seenNonce[r.Nonce] = node
	i.seenNode[node] = struct{}{}

	// (5) The observation. Each failure mode keeps its own message — absent,
	// unreadable and unattributable are three different states and none of
	// them is a pass. The sentinel is WRAPPED (not just quoted) so the driver
	// can errors.Is it — a mismatch must be distinguishable from a gap.
	switch r.State {
	case StateResident:
	case StateAbsent:
		return i.fail(node, fmt.Errorf("%w", ErrNotResident))
	case StateUnknown:
		return i.fail(node, fmt.Errorf("%w", ErrDigestUnknown))
	default:
		return i.fail(node, fmt.Errorf("unknown digest state %q", string(r.State)))
	}
	if !ValidContentDigest(r.ContentDigest) {
		return i.fail(node, fmt.Errorf("%w (resident digest is not sha256:<64 hex>)", ErrDigestUnknown))
	}
	if r.ProvenanceDigest == "" {
		return i.fail(node, fmt.Errorf("%w", ErrNoProvenance))
	}
	// (6) Correlate the LIVE containerd content digest against what this node
	// recorded when it built the ref. A ref that was retagged onto other bytes
	// lands here, and it fails loudly — it is never repaired by fetching
	// something else.
	if r.ContentDigest != r.ProvenanceDigest {
		return i.fail(node, fmt.Errorf("%w (resident %s, provenance %s)", ErrDigestMismatch, r.ContentDigest, r.ProvenanceDigest))
	}

	i.verdicts = append(i.verdicts, Verdict{Node: node, OK: true, ContentDigest: r.ContentDigest})
	return nil
}

// reject records evidence refused on IDENTITY grounds. It does not consume the
// node slot: the legitimate node may still report.
func (i *Inspector) reject(node, format string, args ...any) error {
	reason := fmt.Sprintf(format, args...)
	i.rejected = append(i.rejected, Verdict{Node: node, OK: false, Reason: reason})
	return fmt.Errorf("%s", reason)
}

// fail records evidence accepted as this node's answer but judged negative.
// The error is returned unchanged (and its message recorded as the reason) so
// a wrapped sentinel survives for the driver to errors.Is.
func (i *Inspector) fail(node string, err error) error {
	i.verdicts = append(i.verdicts, Verdict{Node: node, OK: false, Reason: err.Error()})
	return err
}

// Summarize returns the outcome. It is OK only when EVERY challenged node
// returned accepted, positive evidence — a node that never reported at all
// fails the inspection rather than being skipped.
func (i *Inspector) Summarize() Summary {
	s := Summary{Ref: i.ref, RecipeDigest: i.recipe, Asked: len(i.asked), Rejected: i.rejected}

	byNode := make(map[string]Verdict, len(i.verdicts))
	for _, v := range i.verdicts {
		byNode[v.Node] = v
	}
	var missing []string
	for node := range i.asked {
		v, ok := byNode[node]
		if !ok {
			// Requirement: a missing report FAILS. Silence is not consent.
			v = Verdict{Node: node, OK: false, Reason: "no report received from this node"}
			missing = append(missing, node)
		}
		s.Verdicts = append(s.Verdicts, v)
		if v.OK {
			s.Proven++
		}
	}
	sort.Slice(s.Verdicts, func(a, b int) bool { return s.Verdicts[a].Node < s.Verdicts[b].Node })
	sort.Strings(missing)

	switch {
	case s.Proven == s.Asked && s.Proven >= i.minimum:
		s.OK = true
	case len(missing) > 0:
		s.Reason = fmt.Sprintf("no report from %d of %d node(s): %s", len(missing), s.Asked, strings.Join(missing, ", "))
	default:
		s.Reason = fmt.Sprintf("%d of %d node(s) proved the image; %d required", s.Proven, s.Asked, i.minimum)
	}
	return s
}
