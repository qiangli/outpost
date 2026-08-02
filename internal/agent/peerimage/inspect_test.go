package peerimage

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

var (
	testRecipeDigest  = "sha256:" + strings.Repeat("b", 64)
	testContentDigest = "sha256:" + strings.Repeat("a", 64)
	testRef           = "localhost/cluster/demo:v1"
)

// seqNonce mints deterministic nonces for tests.
func seqNonce() NonceFunc {
	n := 0
	return func() (string, error) {
		n++
		return fmt.Sprintf("nonce-%d", n), nil
	}
}

func newTestInspector(t *testing.T, nodes []string, minimum int) *Inspector {
	t.Helper()
	insp, err := NewInspector(testRef, testRecipeDigest, nodes, minimum, seqNonce())
	if err != nil {
		t.Fatalf("NewInspector: %v", err)
	}
	return insp
}

// goodReport builds the report node is EXPECTED to send for its challenge.
func goodReport(insp *Inspector, node string) Report {
	for _, ch := range insp.Challenges() {
		if ch.Node == node {
			return Report{
				Node: node, Ref: ch.Ref, Recipe: ch.Recipe, Nonce: ch.Nonce,
				State: StateResident, ContentDigest: testContentDigest, ProvenanceDigest: testContentDigest,
			}
		}
	}
	return Report{Node: node}
}

// --- construction: the requirement-2 gate ---------------------------------

func TestNewInspector_RejectsDuplicateNodes(t *testing.T) {
	// One host running two virtual backends presents TWO nodes that share the
	// host label — but here identity is the node NAME, and the same name twice
	// is one node. An "N nodes reached" claim built from it is fabricated.
	_, err := NewInspector(testRef, testRecipeDigest, []string{"worker-a", "worker-a"}, 2, seqNonce())
	if !errors.Is(err, ErrNotDistinct) {
		t.Fatalf("duplicate node = %v, want ErrNotDistinct", err)
	}
}

func TestNewInspector_RejectsUnnamedNode(t *testing.T) {
	if _, err := NewInspector(testRef, testRecipeDigest, []string{"worker-a", "  "}, 1, seqNonce()); !errors.Is(err, ErrNotDistinct) {
		t.Fatalf("unnamed node = %v, want ErrNotDistinct", err)
	}
}

func TestNewInspector_RequiresMinimumDistinct(t *testing.T) {
	if _, err := NewInspector(testRef, testRecipeDigest, []string{"worker-a"}, 2, seqNonce()); !errors.Is(err, ErrNotDistinct) {
		t.Fatalf("1 node satisfied a 2-node claim: %v", err)
	}
}

func TestNewInspector_RejectsBadIdentity(t *testing.T) {
	if _, err := NewInspector("", testRecipeDigest, []string{"a"}, 1, nil); err == nil {
		t.Fatal("empty ref accepted")
	}
	if _, err := NewInspector(testRef, "not-a-digest", []string{"a"}, 1, nil); err == nil {
		t.Fatal("malformed recipe digest accepted")
	}
}

func TestInspector_ChallengesAreDistinctAndDeterministic(t *testing.T) {
	insp := newTestInspector(t, []string{"worker-c", "worker-a", "worker-b"}, 2)
	ch := insp.Challenges()
	if len(ch) != 3 || ch[0].Node != "worker-a" || ch[1].Node != "worker-b" || ch[2].Node != "worker-c" {
		t.Fatalf("challenges not sorted/distinct: %+v", ch)
	}
	nonces := map[string]string{}
	for _, c := range ch {
		if c.Ref != testRef || c.Recipe != testRecipeDigest || c.Nonce == "" {
			t.Fatalf("challenge missing identity: %+v", c)
		}
		if owner, dup := nonces[c.Nonce]; dup {
			t.Fatalf("nonce %q issued to both %s and %s", c.Nonce, owner, c.Node)
		}
		nonces[c.Nonce] = c.Node
	}
}

// --- Accept: the requirement-3 refusals ------------------------------------

func TestAccept_RejectsForeignNode(t *testing.T) {
	insp := newTestInspector(t, []string{"worker-a"}, 1)
	// Evidence from a node that was never asked must not pad the count.
	err := insp.Accept(Report{
		Node: "worker-evil", Ref: testRef, Recipe: testRecipeDigest, Nonce: "whatever",
		State: StateResident, ContentDigest: testContentDigest, ProvenanceDigest: testContentDigest,
	})
	if err == nil || !strings.Contains(err.Error(), "foreign node") {
		t.Fatalf("foreign node = %v, want a foreign-node refusal", err)
	}
	if s := insp.Summarize(); s.Proven != 0 {
		t.Fatal("foreign evidence was scored")
	}
}

func TestAccept_RejectsReplayedNonce(t *testing.T) {
	insp := newTestInspector(t, []string{"worker-a", "worker-b"}, 2)
	ra := goodReport(insp, "worker-a")
	if err := insp.Accept(ra); err != nil {
		t.Fatalf("first report: %v", err)
	}
	// worker-b replays worker-a's nonce — one node's answer relabeled as another's.
	rb := goodReport(insp, "worker-b")
	rb.Nonce = ra.Nonce
	err := insp.Accept(rb)
	if err == nil || !strings.Contains(err.Error(), "duplicate evidence") {
		t.Fatalf("replayed nonce = %v, want a duplicate-evidence refusal", err)
	}
}

func TestAccept_RejectsNonceIssuedToAnotherNode(t *testing.T) {
	insp := newTestInspector(t, []string{"worker-a", "worker-b"}, 2)
	// worker-b presents the nonce that was issued to worker-a (never spent).
	rb := goodReport(insp, "worker-b")
	for _, ch := range insp.Challenges() {
		if ch.Node == "worker-a" {
			rb.Nonce = ch.Nonce
		}
	}
	err := insp.Accept(rb)
	if err == nil || !strings.Contains(err.Error(), "foreign evidence") {
		t.Fatalf("cross-node nonce = %v, want a foreign-evidence refusal", err)
	}
}

func TestAccept_RejectsDuplicateNodeReport(t *testing.T) {
	insp := newTestInspector(t, []string{"worker-a", "worker-b"}, 2)
	if err := insp.Accept(goodReport(insp, "worker-a")); err != nil {
		t.Fatalf("first report: %v", err)
	}
	// A second report from the same node, even well-formed, is a duplicate.
	again := goodReport(insp, "worker-a")
	err := insp.Accept(again)
	if err == nil || !strings.Contains(err.Error(), "duplicate evidence") {
		t.Fatalf("second report = %v, want a duplicate-evidence refusal", err)
	}
	if s := insp.Summarize(); s.Proven != 1 {
		t.Fatalf("a duplicate report inflated the proof count: %+v", s)
	}
}

func TestAccept_RejectsIdentityMismatch(t *testing.T) {
	insp := newTestInspector(t, []string{"worker-a"}, 1)
	// Right node, right nonce — but about a different ref. The evidence cannot
	// be attributed to what was asked.
	r := goodReport(insp, "worker-a")
	r.Ref = "localhost/cluster/other:v9"
	if err := insp.Accept(r); err == nil || !strings.Contains(err.Error(), "identity mismatch") {
		t.Fatalf("wrong-ref report = %v, want an identity-mismatch refusal", err)
	}
	// Same for the recipe.
	insp2 := newTestInspector(t, []string{"worker-a"}, 1)
	r2 := goodReport(insp2, "worker-a")
	r2.Recipe = "sha256:" + strings.Repeat("f", 64)
	if err := insp2.Accept(r2); err == nil || !strings.Contains(err.Error(), "identity mismatch") {
		t.Fatalf("wrong-recipe report = %v, want an identity-mismatch refusal", err)
	}
}

func TestAccept_RejectedEvidenceDoesNotConsumeTheNodeSlot(t *testing.T) {
	insp := newTestInspector(t, []string{"worker-a"}, 1)
	// A forged report (wrong nonce) is refused...
	forged := goodReport(insp, "worker-a")
	forged.Nonce = "forged"
	if err := insp.Accept(forged); err == nil {
		t.Fatal("forged report accepted")
	}
	// ...and the legitimate node can still report — the refusal did not burn it.
	if err := insp.Accept(goodReport(insp, "worker-a")); err != nil {
		t.Fatalf("legitimate report after a refused forgery: %v", err)
	}
	if s := insp.Summarize(); !s.OK {
		t.Fatalf("summary not OK after one good report: %+v", s)
	}
}

// --- Accept: the observation (evidence invariant + digest correlation) -----

func TestAccept_AbsentIsNotAPass(t *testing.T) {
	insp := newTestInspector(t, []string{"worker-a"}, 1)
	r := goodReport(insp, "worker-a")
	r.State = StateAbsent
	r.ContentDigest = ""
	r.ProvenanceDigest = ""
	if err := insp.Accept(r); !errors.Is(err, ErrNotResident) {
		t.Fatalf("absent = %v, want ErrNotResident", err)
	}
	if s := insp.Summarize(); s.OK {
		t.Fatal("an absent image satisfied the inspection")
	}
}

func TestAccept_UnknownIsNotAbsentNotAPass(t *testing.T) {
	insp := newTestInspector(t, []string{"worker-a"}, 1)
	r := goodReport(insp, "worker-a")
	r.State = StateUnknown
	if err := insp.Accept(r); !errors.Is(err, ErrDigestUnknown) {
		t.Fatalf("unknown = %v, want ErrDigestUnknown", err)
	}
}

func TestAccept_MalformedResidentDigestIsUnknown(t *testing.T) {
	insp := newTestInspector(t, []string{"worker-a"}, 1)
	r := goodReport(insp, "worker-a")
	r.ContentDigest = "i-am-not-a-digest"
	if err := insp.Accept(r); !errors.Is(err, ErrDigestUnknown) {
		t.Fatalf("malformed digest = %v, want ErrDigestUnknown", err)
	}
}

func TestAccept_NoProvenanceIsNotAPass(t *testing.T) {
	insp := newTestInspector(t, []string{"worker-a"}, 1)
	r := goodReport(insp, "worker-a")
	r.ProvenanceDigest = "" // resident, but unattributable to any recipe
	if err := insp.Accept(r); !errors.Is(err, ErrNoProvenance) {
		t.Fatalf("no provenance = %v, want ErrNoProvenance", err)
	}
}

// The live containerd digest MUST correlate with the recorded provenance. A
// ref retagged onto different bytes fails loudly here — the requirement-4 gate.
func TestAccept_DigestMismatchFailsLoudly(t *testing.T) {
	insp := newTestInspector(t, []string{"worker-a"}, 1)
	r := goodReport(insp, "worker-a")
	r.ContentDigest = "sha256:" + strings.Repeat("e", 64) // resident NOW
	r.ProvenanceDigest = testContentDigest                // recorded at build
	err := insp.Accept(r)
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("mismatch = %v, want ErrDigestMismatch", err)
	}
	if s := insp.Summarize(); s.OK {
		t.Fatal("a digest mismatch satisfied the inspection")
	}
}

func TestAccept_ValidEvidenceAccepted(t *testing.T) {
	insp := newTestInspector(t, []string{"worker-a", "worker-b"}, 2)
	if err := insp.Accept(goodReport(insp, "worker-a")); err != nil {
		t.Fatalf("worker-a: %v", err)
	}
	if err := insp.Accept(goodReport(insp, "worker-b")); err != nil {
		t.Fatalf("worker-b: %v", err)
	}
	s := insp.Summarize()
	if !s.OK || s.Proven != 2 || s.Asked != 2 {
		t.Fatalf("summary: %+v", s)
	}
}

// --- Summarize: absence of evidence is failure ------------------------------

func TestSummarize_MissingReportFails(t *testing.T) {
	insp := newTestInspector(t, []string{"worker-a", "worker-b"}, 2)
	// Only worker-a ever reports. worker-b's silence must fail the inspection,
	// not be skipped.
	if err := insp.Accept(goodReport(insp, "worker-a")); err != nil {
		t.Fatal(err)
	}
	s := insp.Summarize()
	if s.OK {
		t.Fatal("a node that never reported was treated as satisfied")
	}
	if !strings.Contains(s.Reason, "no report") {
		t.Fatalf("reason does not name the missing report: %q", s.Reason)
	}
	if s.Proven != 1 || s.Asked != 2 {
		t.Fatalf("summary: %+v", s)
	}
}

func TestSummarize_RejectionsAreSurfaced(t *testing.T) {
	insp := newTestInspector(t, []string{"worker-a"}, 1)
	_ = insp.Accept(Report{Node: "worker-evil", Nonce: "x"}) // foreign — rejected
	if err := insp.Accept(goodReport(insp, "worker-a")); err != nil {
		t.Fatal(err)
	}
	s := insp.Summarize()
	if !s.OK {
		t.Fatalf("one good report should satisfy: %+v", s)
	}
	if len(s.Rejected) != 1 {
		t.Fatalf("the rejected forgery was not surfaced: %+v", s.Rejected)
	}
}

func TestSummarize_MinimumGate(t *testing.T) {
	// Three challenged, minimum 3 — all must prove.
	insp := newTestInspector(t, []string{"a", "b", "c"}, 3)
	for _, n := range []string{"a", "b", "c"} {
		if err := insp.Accept(goodReport(insp, n)); err != nil {
			t.Fatalf("%s: %v", n, err)
		}
	}
	if s := insp.Summarize(); !s.OK || s.Proven != 3 {
		t.Fatalf("all three proved but summary not OK: %+v", s)
	}
}
