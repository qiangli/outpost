package recipebuilder

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	present            bool
	presenceErr        error
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
	f.present = true
	return nil
}
func (f *fakeRunner) ImagePresent(context.Context, string, string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.present, f.presenceErr
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

func TestBuilder_RebuildsWhenRuntimeLostUnchangedImage(t *testing.T) {
	recipe := "name: agg\nlocal_ref: localhost/cluster/agg\ncontext_type: local\ncontext_path: /tmp/x\ndockerfile: Dockerfile\ncontext_sha256: v1\n"
	var mu sync.Mutex
	srv := recipeServer(&recipe, &mu)
	defer srv.Close()

	fr := &fakeRunner{}
	b := newTestBuilder(t, srv.URL, fr)
	b.tick(context.Background())

	fr.mu.Lock()
	fr.present = false // inner containerd was recreated
	fr.mu.Unlock()
	b.tick(context.Background())

	if fr.builds != 2 || fr.loads != 2 {
		t.Fatalf("builds=%d loads=%d, want 2/2 after runtime image loss", fr.builds, fr.loads)
	}
}

func TestBuilder_FailsClosedWhenPresenceCannotBeChecked(t *testing.T) {
	recipe := "name: agg\nlocal_ref: localhost/cluster/agg\ncontext_type: local\ncontext_path: /tmp/x\ndockerfile: Dockerfile\ncontext_sha256: v1\n"
	var mu sync.Mutex
	srv := recipeServer(&recipe, &mu)
	defer srv.Close()

	fr := &fakeRunner{}
	b := newTestBuilder(t, srv.URL, fr)
	b.tick(context.Background())

	fr.mu.Lock()
	fr.present = false
	fr.presenceErr = fmt.Errorf("runtime unavailable")
	fr.mu.Unlock()
	b.tick(context.Background())

	if fr.builds != 1 || fr.loads != 1 {
		t.Fatalf("builds=%d loads=%d, want no speculative rebuild on presence-check error", fr.builds, fr.loads)
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

func inlineRecipe(t *testing.T, entries map[string][]byte) Recipe {
	t.Helper()
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	for name, body := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(archive.Bytes())
	return Recipe{
		Name: "inline", LocalRef: "localhost/cluster/inline",
		ContextType: "inline_tar", ContextArchive: base64.StdEncoding.EncodeToString(archive.Bytes()),
		ContextSha256: fmt.Sprintf("%x", sum), Dockerfile: "Dockerfile",
	}
}

func TestBuilder_InlineTarVerifiesExtractsBuildsAndLoads(t *testing.T) {
	r := inlineRecipe(t, map[string][]byte{
		"Dockerfile": []byte("FROM scratch\n"),
		"app/main":   []byte("payload"),
	})
	fr := &fakeRunner{}
	b := New(Config{
		RuntimeContainer: "n-runtime", Platform: "linux/amd64",
		WorkDir: t.TempDir(), Runner: fr,
	})
	if err := b.buildOne(context.Background(), r); err != nil {
		t.Fatalf("buildOne() error = %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(fr.lastCtx, "app", "main")); err != nil || string(got) != "payload" {
		t.Fatalf("extracted payload = %q, err=%v", got, err)
	}
	if fr.builds != 1 || fr.loads != 1 || !fr.present {
		t.Fatalf("builds=%d loads=%d present=%v", fr.builds, fr.loads, fr.present)
	}
}

func TestBuilder_InlineTarRejectsDigestMismatchBeforeBuild(t *testing.T) {
	r := inlineRecipe(t, map[string][]byte{"Dockerfile": []byte("FROM scratch\n")})
	r.ContextSha256 = string(bytes.Repeat([]byte{'0'}, 64))
	fr := &fakeRunner{}
	b := New(Config{RuntimeContainer: "n-runtime", WorkDir: t.TempDir(), Runner: fr})
	if err := b.buildOne(context.Background(), r); err == nil {
		t.Fatal("buildOne() accepted a digest mismatch")
	}
	if fr.builds != 0 || fr.loads != 0 {
		t.Fatalf("digest mismatch reached runner: builds=%d loads=%d", fr.builds, fr.loads)
	}
}

func TestExtractInlineArchiveRejectsTraversalAndLinks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header tar.Header
	}{
		{name: "parent traversal", header: tar.Header{Name: "../escape", Mode: 0o644, Typeflag: tar.TypeReg}},
		{name: "absolute", header: tar.Header{Name: "/escape", Mode: 0o644, Typeflag: tar.TypeReg}},
		{name: "portable traversal", header: tar.Header{Name: `..\escape`, Mode: 0o644, Typeflag: tar.TypeReg}},
		{name: "symlink", header: tar.Header{Name: "link", Linkname: "/tmp/escape", Typeflag: tar.TypeSymlink}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			tw := tar.NewWriter(&buf)
			if err := tw.WriteHeader(&tc.header); err != nil {
				t.Fatal(err)
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			if err := extractInlineArchive(buf.Bytes(), t.TempDir()); err == nil {
				t.Fatal("unsafe archive was accepted")
			}
		})
	}
}

func TestDecodeInlineArchiveRejectsOversize(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString(make([]byte, maxInlineArchiveBytes+1))
	if _, err := decodeInlineArchive(encoded, string(bytes.Repeat([]byte{'0'}, 64))); err == nil {
		t.Fatal("oversize archive was accepted")
	}
}

func TestExtractInlineArchiveRejectsExpandedSize(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	h := &tar.Header{Name: "huge", Mode: 0o644, Typeflag: tar.TypeReg, Size: maxExpandedBytes + 1}
	if err := tw.WriteHeader(h); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err == nil {
		t.Fatal("expected tar writer to reject missing huge body")
	}
	if err := extractInlineArchive(buf.Bytes(), t.TempDir()); err == nil {
		t.Fatal("oversize expanded archive was accepted")
	}
}
