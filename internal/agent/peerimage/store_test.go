package peerimage

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testRecipe is a minimal valid recipe document. context_type=local keeps it
// build-free; the digest fields are filled in by the tests that need them.
func testRecipe(name string) string {
	return "name: " + name + "\n" +
		"tag: v1\n" +
		"local_ref: localhost/cluster/" + name + "\n" +
		"context_type: local\n" +
		"context_path: /tmp/x\n" +
		"dockerfile: Dockerfile\n" +
		"context_sha256: \"abc123\"\n"
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(filepath.Join(t.TempDir(), "store"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestStore_PublishAndReadBack(t *testing.T) {
	s := newTestStore(t)
	pub, err := s.Publish("demo", testRecipe("demo"))
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if pub.Name != "demo" || pub.Ref != "localhost/cluster/demo:v1" {
		t.Fatalf("publication identity wrong: %+v", pub)
	}
	if !ValidRecipeDigest(pub.Digest) {
		t.Fatalf("publication digest is not sha256:<64 hex>: %q", pub.Digest)
	}
	body, rec, err := s.Recipe("demo")
	if err != nil {
		t.Fatalf("Recipe: %v", err)
	}
	if body != testRecipe("demo") {
		t.Fatalf("stored body drifted:\n%q\nwant:\n%q", body, testRecipe("demo"))
	}
	if rec.Ref() != pub.Ref || rec.Digest() != pub.Digest {
		t.Fatalf("parsed recipe disagrees with publication: %q vs %q", rec.Digest(), pub.Digest)
	}
}

func TestStore_PublishRejectsInvalid(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Publish("demo", "name: demo\n"); err == nil {
		t.Fatal("an incomplete recipe was accepted")
	}
	if _, err := s.Publish("demo", "name: other\nlocal_ref: localhost/cluster/other\ncontext_type: local\ncontext_path: /x\ndockerfile: Dockerfile\n"); err == nil {
		t.Fatal("a name/doc mismatch was accepted")
	}
	if _, err := s.Publish("../escape", testRecipe("demo")); err == nil {
		t.Fatal("a path-traversal name was accepted")
	}
	// A rejected publish must not have left a file behind.
	if _, _, err := s.Recipe("demo"); !errors.Is(err, ErrNoRecipe) {
		t.Fatalf("rejected publish left state behind: %v", err)
	}
}

func TestStore_RecipeMissingIsErrNoRecipe(t *testing.T) {
	s := newTestStore(t)
	_, _, err := s.Recipe("never-published")
	if !errors.Is(err, ErrNoRecipe) {
		t.Fatalf("missing recipe = %v, want ErrNoRecipe", err)
	}
}

func TestStore_ListSortsAndFailsOnCorruption(t *testing.T) {
	s := newTestStore(t)
	for _, n := range []string{"zeta", "alpha", "mid"} {
		if _, err := s.Publish(n, testRecipe(n)); err != nil {
			t.Fatalf("Publish %s: %v", n, err)
		}
	}
	pubs, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(pubs) != 3 || pubs[0].Name != "alpha" || pubs[1].Name != "mid" || pubs[2].Name != "zeta" {
		t.Fatalf("List not sorted by name: %+v", pubs)
	}

	// A truncated/corrupt entry must fail the WHOLE listing — a listing that
	// silently drops a recipe would let "the peer doesn't have it" mean "we
	// failed to read it".
	if err := os.WriteFile(s.recipePath("alpha"), []byte("name: alpha\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.List(); err == nil {
		t.Fatal("a corrupt recipe was silently dropped from the listing")
	}
}

func TestStore_ProvenanceRoundTrip(t *testing.T) {
	s := newTestStore(t)
	digest := "sha256:" + strings.Repeat("a", 64)
	recipeDigest := "sha256:" + strings.Repeat("b", 64)
	p := Provenance{
		Node: "worker-a", Ref: "localhost/cluster/demo:v1", Recipe: "demo",
		RecipeDigest: recipeDigest, ContentDigest: digest,
	}
	if err := s.PutProvenance(p); err != nil {
		t.Fatalf("PutProvenance: %v", err)
	}
	got, ok, err := s.Provenance(p.Ref)
	if err != nil || !ok {
		t.Fatalf("Provenance: ok=%v err=%v", ok, err)
	}
	if got.ContentDigest != digest || got.RecipeDigest != recipeDigest || got.Node != "worker-a" {
		t.Fatalf("provenance drifted: %+v", got)
	}
}

func TestStore_ProvenanceRefusesInvalidDigests(t *testing.T) {
	s := newTestStore(t)
	base := Provenance{
		Node: "worker-a", Ref: "localhost/cluster/demo:v1", Recipe: "demo",
		RecipeDigest:  "sha256:" + strings.Repeat("b", 64),
		ContentDigest: "sha256:" + strings.Repeat("a", 64),
	}
	noContent := base
	noContent.ContentDigest = "not-a-digest"
	if err := s.PutProvenance(noContent); err == nil {
		t.Fatal("provenance with an unreadable content digest was recorded")
	}
	noRecipe := base
	noRecipe.RecipeDigest = ""
	if err := s.PutProvenance(noRecipe); err == nil {
		t.Fatal("provenance with no recipe digest was recorded")
	}
	// Nothing valid was ever written, so a read must report "no record".
	if _, ok, err := s.Provenance(base.Ref); err != nil || ok {
		t.Fatalf("rejected provenance left a record: ok=%v err=%v", ok, err)
	}
}

func TestStore_ProvenanceAbsentIsNotAnError(t *testing.T) {
	s := newTestStore(t)
	_, ok, err := s.Provenance("localhost/cluster/ghost:v1")
	if err != nil {
		t.Fatalf("absent provenance returned an error: %v", err)
	}
	if ok {
		t.Fatal("absent provenance reported as present")
	}
}
