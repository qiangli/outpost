package peerimage

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

// recipeIndexServer serves a peer's IndexHandler with one recipe published.
func recipeIndexServer(t *testing.T, name, body string) *httptest.Server {
	t.Helper()
	peer := &Service{
		Identity: NodeIdentity{Name: "peer-b"},
		Store:    newTestStore(t),
	}
	if body != "" {
		if _, err := peer.Store.Publish(name, body); err != nil {
			// The invalid-recipe test publishes something the store rejects;
			// write it directly so the server still serves it.
			if werr := writeFileAtomic(peer.Store.recipePath(name), []byte(body), 0o600); werr != nil {
				t.Fatal(werr)
			}
		}
	}
	srv := httptest.NewServer(peer.IndexHandler())
	t.Cleanup(srv.Close)
	return srv
}

// fakeRuntime is a scripted Runtime. Each call to ResidentDigest pops the next
// script entry; when the script runs out it replays the last entry.
type fakeRuntime struct {
	script []rtStep
	idx    int
	calls  int
}

type rtStep struct {
	state  DigestState
	digest string
	err    error
}

func (f *fakeRuntime) ResidentDigest(context.Context, string) (DigestState, string, error) {
	f.calls++
	if len(f.script) == 0 {
		return StateUnknown, "", errors.New("fakeRuntime: unscripted call")
	}
	if f.idx >= len(f.script) {
		f.idx = len(f.script) - 1
	}
	s := f.script[f.idx]
	f.idx++
	return s.state, s.digest, s.err
}

// fakeBuilder records builds and can be told to fail.
type fakeBuilder struct {
	builds int
	err    error
}

func (f *fakeBuilder) Materialize(context.Context, string) error {
	f.builds++
	return f.err
}

func newTestService(t *testing.T, rt Runtime, b Builder) *Service {
	t.Helper()
	return &Service{
		Identity: NodeIdentity{Name: "worker-a"},
		Store:    newTestStore(t),
		Runtime:  rt,
		Build:    b,
	}
}

func TestService_CheckGates(t *testing.T) {
	// A nil-service receiver must report "not enabled", not panic.
	var nilSvc *Service
	if _, err := nilSvc.Publications(); err == nil {
		t.Fatal("a nil service answered as if enabled")
	}
	// An unnamed node can never emit attributable evidence.
	s := &Service{Store: newTestStore(t)}
	if _, err := s.Publications(); err == nil {
		t.Fatal("a service with no node identity was accepted")
	}
	// A service with no store cannot publish.
	s2 := &Service{Identity: NodeIdentity{Name: "n"}}
	if _, err := s2.Publish(context.Background(), "x", testRecipe("x")); err == nil {
		t.Fatal("a service with no store published")
	}
}

func TestService_PublishAndList(t *testing.T) {
	s := newTestService(t, &fakeRuntime{}, &fakeBuilder{})
	if _, err := s.Publish(context.Background(), "demo", testRecipe("demo")); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	pubs, err := s.Publications()
	if err != nil {
		t.Fatalf("Publications: %v", err)
	}
	if len(pubs) != 1 || pubs[0].Name != "demo" {
		t.Fatalf("Publications: %+v", pubs)
	}
}

// ---------------------------------------------------------------------------
// mesh-resolve — the distinctness gate (requirement 2)
// ---------------------------------------------------------------------------

func resolveSvc(t *testing.T, peers []PeerRef, err error) *Service {
	t.Helper()
	s := newTestService(t, &fakeRuntime{}, &fakeBuilder{})
	s.Resolver = func(string) ([]PeerRef, error) { return peers, err }
	return s
}

func TestMeshResolve_EmptyRegistryIsNotSuccess(t *testing.T) {
	s := resolveSvc(t, nil, nil)
	_, err := s.MeshResolve(context.Background(), "recipes", 1)
	if !errors.Is(err, ErrNoPeers) {
		t.Fatalf("empty registry = %v, want ErrNoPeers", err)
	}
}

func TestMeshResolve_NilResolverIsNotSuccess(t *testing.T) {
	s := newTestService(t, &fakeRuntime{}, &fakeBuilder{})
	if _, err := s.MeshResolve(context.Background(), "recipes", 1); err == nil {
		t.Fatal("a nil resolver produced an empty success")
	}
}

func TestMeshResolve_RequiresDistinctPeers(t *testing.T) {
	// Two entries, ONE identity — a host running two backends answers twice
	// with the same peer id. "Reaches 2 peers" must not be provable from one.
	dupes := []PeerRef{
		{Host: "mac-pro", PeerID: "QmSamePeer"},
		{Host: "mac-pro", PeerID: "QmSamePeer"},
	}
	s := resolveSvc(t, dupes, nil)
	res, err := s.MeshResolve(context.Background(), "recipes", 2)
	if !errors.Is(err, ErrNotDistinct) {
		t.Fatalf("duplicated peer inflated the count: %v", err)
	}
	if res.Distinct != 0 {
		t.Fatalf("a failed resolve reported distinct=%d", res.Distinct)
	}
}

func TestMeshResolve_DropsUnidentifiablePeers(t *testing.T) {
	// A peer with no id cannot be dialed, so it cannot count as reached.
	peers := []PeerRef{{Host: "ghost"}, {Host: "real", PeerID: "QmReal"}}
	s := resolveSvc(t, peers, nil)
	res, err := s.MeshResolve(context.Background(), "recipes", 1)
	if err != nil {
		t.Fatalf("MeshResolve: %v", err)
	}
	if res.Distinct != 1 || res.Peers[0].PeerID != "QmReal" {
		t.Fatalf("unidentifiable peer was counted: %+v", res)
	}
}

func TestMeshResolve_MinimumEnforced(t *testing.T) {
	peers := []PeerRef{{Host: "a", PeerID: "QmA"}, {Host: "b", PeerID: "QmB"}}
	s := resolveSvc(t, peers, nil)
	if _, err := s.MeshResolve(context.Background(), "recipes", 3); !errors.Is(err, ErrNotDistinct) {
		t.Fatalf("2 peers satisfied a 3-peer claim: %v", err)
	}
	res, err := s.MeshResolve(context.Background(), "recipes", 2)
	if err != nil || res.Distinct != 2 {
		t.Fatalf("2 distinct peers: res=%+v err=%v", res, err)
	}
}

// ---------------------------------------------------------------------------
// ensure — the digest-correlation state machine (requirement 4)
// ---------------------------------------------------------------------------

func TestEnsure_NoRecipeIsFailure(t *testing.T) {
	s := newTestService(t, &fakeRuntime{}, &fakeBuilder{})
	if _, err := s.Ensure(context.Background(), "ghost"); !errors.Is(err, ErrNoRecipe) {
		t.Fatalf("unknown recipe = %v, want ErrNoRecipe", err)
	}
}

func TestEnsure_NoRuntimeIsFailure(t *testing.T) {
	s := newTestService(t, nil, &fakeBuilder{})
	if _, err := s.Publish(context.Background(), "demo", testRecipe("demo")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Ensure(context.Background(), "demo"); err == nil {
		t.Fatal("ensure succeeded with no runtime to confirm residency")
	}
}

// Resident + provenance agrees → satisfied, no rebuild.
func TestEnsure_AlreadyResidentAndProven(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	s := newTestService(t, &fakeRuntime{script: []rtStep{{state: StateResident, digest: digest}}}, &fakeBuilder{})
	pub, err := s.Publish(context.Background(), "demo", testRecipe("demo"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Store.PutProvenance(Provenance{
		Node: "worker-a", Ref: pub.Ref, Recipe: "demo",
		RecipeDigest: pub.Digest, ContentDigest: digest,
	}); err != nil {
		t.Fatal(err)
	}
	res, err := s.Ensure(context.Background(), "demo")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if res.Built {
		t.Fatal("rebuilt an already-proven image")
	}
	if res.State != StateResident || res.ContentDigest != digest {
		t.Fatalf("result: %+v", res)
	}
}

// Resident + provenance DISAGREES → loud failure, and NO silent repair.
func TestEnsure_DigestMismatchFailsLoudlyWithoutRepair(t *testing.T) {
	live := "sha256:" + strings.Repeat("c", 64)     // what containerd has NOW
	recorded := "sha256:" + strings.Repeat("a", 64) // what this node built
	b := &fakeBuilder{}
	s := newTestService(t, &fakeRuntime{script: []rtStep{{state: StateResident, digest: live}}}, b)
	pub, err := s.Publish(context.Background(), "demo", testRecipe("demo"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Store.PutProvenance(Provenance{
		Node: "worker-a", Ref: pub.Ref, Recipe: "demo",
		RecipeDigest: pub.Digest, ContentDigest: recorded,
	}); err != nil {
		t.Fatal(err)
	}
	_, err = s.Ensure(context.Background(), "demo")
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("mismatch = %v, want ErrDigestMismatch", err)
	}
	if b.builds != 0 {
		t.Fatal("a digest mismatch was silently 'repaired' by rebuilding")
	}
}

// Resident but NO provenance → the bytes cannot be attributed, so rebuild from
// the verified recipe (the only way this node can attribute what it runs).
func TestEnsure_ResidentWithoutProvenanceRebuilds(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	rt := &fakeRuntime{script: []rtStep{
		{state: StateResident, digest: digest}, // before: resident, no provenance
		{state: StateResident, digest: digest}, // after build: read back
	}}
	b := &fakeBuilder{}
	s := newTestService(t, rt, b)
	if _, err := s.Publish(context.Background(), "demo", testRecipe("demo")); err != nil {
		t.Fatal(err)
	}
	res, err := s.Ensure(context.Background(), "demo")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if b.builds != 1 || !res.Built {
		t.Fatalf("unprovenanced resident image was not rebuilt: builds=%d res=%+v", b.builds, res)
	}
	// Provenance must now be recorded FROM the read-back digest.
	if _, ok, _ := s.Store.Provenance(res.Ref); !ok {
		t.Fatal("no provenance recorded after rebuild")
	}
}

// Unknown digest is NOT absent and NOT a pass — it must not trigger a build.
func TestEnsure_UnknownDigestIsNotAbsent(t *testing.T) {
	b := &fakeBuilder{}
	s := newTestService(t, &fakeRuntime{script: []rtStep{{state: StateUnknown}}}, b)
	if _, err := s.Publish(context.Background(), "demo", testRecipe("demo")); err != nil {
		t.Fatal(err)
	}
	_, err := s.Ensure(context.Background(), "demo")
	if !errors.Is(err, ErrDigestUnknown) {
		t.Fatalf("unknown digest = %v, want ErrDigestUnknown", err)
	}
	if b.builds != 0 {
		t.Fatal("'could not determine digest' was collapsed into 'absent' and rebuilt")
	}
}

// A runtime that cannot be consulted is an error — never "absent, so build".
func TestEnsure_UnreachableRuntimeDoesNotBuild(t *testing.T) {
	b := &fakeBuilder{}
	rt := &fakeRuntime{script: []rtStep{{err: errors.New("boom")}}}
	s := newTestService(t, rt, b)
	if _, err := s.Publish(context.Background(), "demo", testRecipe("demo")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Ensure(context.Background(), "demo"); err == nil {
		t.Fatal("an unreachable runtime was treated as a buildable absence")
	}
	if b.builds != 0 {
		t.Fatal("built against a runtime that never answered")
	}
}

// Absent → build → read the digest BACK → record provenance from the read-back.
func TestEnsure_AbsentBuildsAndRecordsFromReadBack(t *testing.T) {
	digest := "sha256:" + strings.Repeat("d", 64)
	rt := &fakeRuntime{script: []rtStep{
		{state: StateAbsent},                   // not resident
		{state: StateResident, digest: digest}, // after build
	}}
	b := &fakeBuilder{}
	s := newTestService(t, rt, b)
	pub, err := s.Publish(context.Background(), "demo", testRecipe("demo"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Ensure(context.Background(), "demo")
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !res.Built || res.State != StateResident || res.ContentDigest != digest {
		t.Fatalf("result: %+v", res)
	}
	prov, ok, err := s.Store.Provenance(pub.Ref)
	if err != nil || !ok {
		t.Fatalf("provenance not recorded: ok=%v err=%v", ok, err)
	}
	if prov.ContentDigest != digest || prov.RecipeDigest != pub.Digest || prov.Node != "worker-a" {
		t.Fatalf("provenance recorded from something other than the read-back: %+v", prov)
	}
}

// A build that "succeeds" without producing a readable resident digest FAILS.
func TestEnsure_BuildWithoutReadableDigestFails(t *testing.T) {
	rt := &fakeRuntime{script: []rtStep{
		{state: StateAbsent},
		{state: StateAbsent}, // still absent after the build claimed success
	}}
	s := newTestService(t, rt, &fakeBuilder{})
	if _, err := s.Publish(context.Background(), "demo", testRecipe("demo")); err != nil {
		t.Fatal(err)
	}
	_, err := s.Ensure(context.Background(), "demo")
	if !errors.Is(err, ErrNotResident) {
		t.Fatalf("phantom build = %v, want ErrNotResident", err)
	}
}

// A malformed digest read back after build is unknown, never recorded.
func TestEnsure_MalformedDigestAfterBuildFails(t *testing.T) {
	rt := &fakeRuntime{script: []rtStep{
		{state: StateAbsent},
		{state: StateResident, digest: "garbage"},
	}}
	s := newTestService(t, rt, &fakeBuilder{})
	if _, err := s.Publish(context.Background(), "demo", testRecipe("demo")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Ensure(context.Background(), "demo"); !errors.Is(err, ErrDigestUnknown) {
		t.Fatalf("malformed read-back = %v, want ErrDigestUnknown", err)
	}
}

// No builder configured → say so, rather than reporting success.
func TestEnsure_NoBuilderIsFailure(t *testing.T) {
	s := newTestService(t, &fakeRuntime{script: []rtStep{{state: StateAbsent}}}, nil)
	if _, err := s.Publish(context.Background(), "demo", testRecipe("demo")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Ensure(context.Background(), "demo"); err == nil {
		t.Fatal("ensure reported success with no builder")
	}
}

// ---------------------------------------------------------------------------
// report — identity binding (requirement 2)
// ---------------------------------------------------------------------------

func reportSvc(t *testing.T, rt Runtime) *Service {
	t.Helper()
	return newTestService(t, rt, &fakeBuilder{})
}

func goodChallenge() Challenge {
	return Challenge{
		Node: "worker-a", Ref: "localhost/cluster/demo:v1",
		Recipe: "sha256:" + strings.Repeat("b", 64), Nonce: "nonce-1",
	}
}

func TestReport_RefusesChallengeForAnotherNode(t *testing.T) {
	s := reportSvc(t, &fakeRuntime{script: []rtStep{{state: StateResident, digest: "sha256:" + strings.Repeat("a", 64)}}})
	ch := goodChallenge()
	ch.Node = "worker-b" // addressed elsewhere
	rep, err := s.Report(context.Background(), ch)
	if err == nil {
		t.Fatal("answered a challenge addressed to a different node")
	}
	if rep.Node == "worker-b" {
		t.Fatal("a report was attributed to the wrong node")
	}
}

func TestReport_RequiresNonce(t *testing.T) {
	s := reportSvc(t, &fakeRuntime{})
	ch := goodChallenge()
	ch.Nonce = ""
	if _, err := s.Report(context.Background(), ch); err == nil {
		t.Fatal("an unbound report (no nonce) was emitted")
	}
}

func TestReport_RequiresWellFormedIdentity(t *testing.T) {
	s := reportSvc(t, &fakeRuntime{})
	ch := goodChallenge()
	ch.Ref = ""
	if _, err := s.Report(context.Background(), ch); err == nil {
		t.Fatal("a report with no ref was emitted")
	}
	ch = goodChallenge()
	ch.Recipe = "not-a-digest"
	if _, err := s.Report(context.Background(), ch); err == nil {
		t.Fatal("a report with a malformed recipe digest was emitted")
	}
}

// The runtime failing is reported as unknown WITH the error — never absent,
// never resident.
func TestReport_RuntimeErrorIsUnknownNotAbsent(t *testing.T) {
	s := reportSvc(t, &fakeRuntime{script: []rtStep{{err: errors.New("containerd down")}}})
	rep, err := s.Report(context.Background(), goodChallenge())
	if err == nil {
		t.Fatal("a runtime failure produced a clean report")
	}
	if rep.State != StateUnknown {
		t.Fatalf("runtime failure reported as %q, want unknown", rep.State)
	}
	if rep.State == StateAbsent {
		t.Fatal("an unreachable runtime was reported as an absent image")
	}
}

func TestReport_EchoesThisNodesIdentity(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	s := reportSvc(t, &fakeRuntime{script: []rtStep{{state: StateResident, digest: digest}}})
	rep, err := s.Report(context.Background(), goodChallenge())
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if rep.Node != "worker-a" || rep.Ref != goodChallenge().Ref || rep.Recipe != goodChallenge().Recipe || rep.Nonce != goodChallenge().Nonce {
		t.Fatalf("identity not echoed verbatim: %+v", rep)
	}
	if rep.State != StateResident || rep.ContentDigest != digest {
		t.Fatalf("observation: %+v", rep)
	}
}

// ---------------------------------------------------------------------------
// FetchRecipe — pulling a peer's recipe through a mesh forward
// ---------------------------------------------------------------------------

func TestFetchRecipe_FetchesValidatesAndStores(t *testing.T) {
	// A peer's recipe index, served over a loopback "forward" address.
	srv := recipeIndexServer(t, "demo", testRecipe("demo"))
	defer srv.Close()
	forwardAddr := strings.TrimPrefix(srv.URL, "http://")

	s := newTestService(t, &fakeRuntime{}, &fakeBuilder{})
	pub, err := s.FetchRecipe(context.Background(), forwardAddr, "demo")
	if err != nil {
		t.Fatalf("FetchRecipe: %v", err)
	}
	if pub.Name != "demo" || !ValidRecipeDigest(pub.Digest) {
		t.Fatalf("fetched publication: %+v", pub)
	}
	// The recipe is now in the LOCAL store under its own name.
	if _, _, err := s.Store.Recipe("demo"); err != nil {
		t.Fatalf("fetched recipe was not stored: %v", err)
	}
}

func TestFetchRecipe_RejectsAnInvalidRecipe(t *testing.T) {
	// A peer serving garbage must not result in a stored recipe.
	srv := recipeIndexServer(t, "demo", "name: demo\n") // incomplete
	defer srv.Close()
	forwardAddr := strings.TrimPrefix(srv.URL, "http://")

	s := newTestService(t, &fakeRuntime{}, &fakeBuilder{})
	if _, err := s.FetchRecipe(context.Background(), forwardAddr, "demo"); err == nil {
		t.Fatal("an invalid recipe from a peer was stored")
	}
	if _, _, err := s.Store.Recipe("demo"); !errors.Is(err, ErrNoRecipe) {
		t.Fatal("an invalid recipe from a peer was stored anyway")
	}
}

func TestFetchRecipe_RefusesNonForwardTarget(t *testing.T) {
	s := newTestService(t, &fakeRuntime{}, &fakeBuilder{})
	if _, err := s.FetchRecipe(context.Background(), "", "demo"); err == nil {
		t.Fatal("an empty forward address was accepted")
	}
	if _, err := s.FetchRecipe(context.Background(), "203.0.113.9:9999", "demo"); err == nil {
		// The fetcher must refuse a non-loopback target outright.
		t.Fatal("a non-loopback (off-mesh) recipe source was contacted")
	}
	if _, err := s.FetchRecipe(context.Background(), "127.0.0.1:17777", "../bad"); err == nil {
		t.Fatal("a path-traversal recipe name was accepted")
	}
}
