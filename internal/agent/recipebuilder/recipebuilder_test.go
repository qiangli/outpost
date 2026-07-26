package recipebuilder

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestParseRecipe(t *testing.T) {
	body := `# a recipe
apiVersion: dks/v1
kind: ImageRecipe
name: dks-metrics-aggregator
tag: v0.1.0
local_ref: localhost/cluster/dks-metrics-aggregator
context_type: local
context_path: cloudbox/hub
dockerfile: cmd/dks-metrics-aggregator/Dockerfile
base_images: golang:1.26-alpine gcr.io/distroless/static:nonroot
context_sha256: "abc123"
`
	r := parseRecipe(body)
	if r.Name != "dks-metrics-aggregator" || r.Tag != "v0.1.0" {
		t.Fatalf("name/tag: %+v", r)
	}
	if r.LocalRef != "localhost/cluster/dks-metrics-aggregator" || r.ref() != "localhost/cluster/dks-metrics-aggregator:v0.1.0" {
		t.Fatalf("ref: %q", r.ref())
	}
	if r.ContextType != "local" || r.ContextPath != "cloudbox/hub" || r.Dockerfile != "cmd/dks-metrics-aggregator/Dockerfile" {
		t.Fatalf("context: %+v", r)
	}
	if len(r.BaseImages) != 2 || r.ContextSha256 != "abc123" {
		t.Fatalf("bases/sha: %+v", r)
	}
	if !r.valid() {
		t.Fatal("should be valid")
	}
}

type fakeRunner struct {
	mu                 sync.Mutex
	builds, loads      int
	lastRef, lastPlat  string
	lastCtx, lastDfile string
}

func (f *fakeRunner) Clone(context.Context, string, string, string) error { return nil }
func (f *fakeRunner) Build(_ context.Context, platform, dockerfile, ref, contextDir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.builds++
	f.lastPlat, f.lastDfile, f.lastRef, f.lastCtx = platform, dockerfile, ref, contextDir
	return nil
}
func (f *fakeRunner) Load(context.Context, string, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loads++
	return nil
}

// recipeServer serves a single, swappable recipe under /api/v1/recipes.
func recipeServer(content *string, mu *sync.Mutex) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		c := *content
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"recipes": []map[string]string{{"name": "agg", "content": c}},
		})
	}))
}

func newTestBuilder(t *testing.T, base string, fr *fakeRunner) *Builder {
	t.Helper()
	return New(Config{
		CloudboxBase: base, AccessToken: "tok", RuntimeContainer: "n-runtime",
		Platform: "linux/arm64", Runner: fr,
	})
}

func TestBuilder_BuildsThenSkipsUnchanged(t *testing.T) {
	recipe := "name: agg\nlocal_ref: localhost/cluster/agg\ncontext_type: local\ncontext_path: /tmp/x\ndockerfile: Dockerfile\ncontext_sha256: v1\n"
	var mu sync.Mutex
	srv := recipeServer(&recipe, &mu)
	defer srv.Close()

	fr := &fakeRunner{}
	b := newTestBuilder(t, srv.URL, fr)

	b.tick(context.Background())
	b.tick(context.Background()) // unchanged → must not rebuild

	if fr.builds != 1 || fr.loads != 1 {
		t.Fatalf("builds=%d loads=%d, want 1/1 (skip unchanged)", fr.builds, fr.loads)
	}
	if fr.lastPlat != "linux/arm64" || fr.lastRef != "localhost/cluster/agg:latest" {
		t.Fatalf("build args: plat=%q ref=%q", fr.lastPlat, fr.lastRef)
	}
}

func TestBuilder_RebuildsOnChangedSha(t *testing.T) {
	recipe := "name: agg\nlocal_ref: localhost/cluster/agg\ncontext_type: local\ncontext_path: /tmp/x\ndockerfile: Dockerfile\ncontext_sha256: v1\n"
	var mu sync.Mutex
	srv := recipeServer(&recipe, &mu)
	defer srv.Close()

	fr := &fakeRunner{}
	b := newTestBuilder(t, srv.URL, fr)
	b.tick(context.Background())

	mu.Lock()
	recipe = "name: agg\nlocal_ref: localhost/cluster/agg\ncontext_type: local\ncontext_path: /tmp/x\ndockerfile: Dockerfile\ncontext_sha256: v2\n"
	mu.Unlock()
	b.tick(context.Background())

	if fr.builds != 2 {
		t.Fatalf("builds=%d, want 2 (rebuild on changed sha)", fr.builds)
	}
}

func TestBuilder_SkipsInvalid(t *testing.T) {
	recipe := "name: agg\nlocal_ref: localhost/cluster/agg\ncontext_type: local\n" // no dockerfile/context_path
	var mu sync.Mutex
	srv := recipeServer(&recipe, &mu)
	defer srv.Close()

	fr := &fakeRunner{}
	b := newTestBuilder(t, srv.URL, fr)
	b.tick(context.Background())

	if fr.builds != 0 {
		t.Fatalf("builds=%d, want 0 (invalid recipe skipped)", fr.builds)
	}
}
