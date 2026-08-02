package vknode

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// treeEntry is one member of a synthetic archive built entirely in-test — no
// live downloads reach the gate.
type treeEntry struct {
	name     string
	typeflag byte
	mode     int64 // unix perm/type bits as stored in the archive
	body     []byte
	linkname string
	sizeHdr  int64 // when >0, the DECLARED tar size (overrides len(body))
}

func tarGzipTree(t *testing.T, entries []treeEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		size := int64(len(e.body))
		if e.sizeHdr > 0 {
			size = e.sizeHdr
		}
		hdr := &tar.Header{Name: e.name, Typeflag: e.typeflag, Mode: e.mode, Size: size, Linkname: e.linkname}
		if e.typeflag == tar.TypeSymlink || e.typeflag == tar.TypeLink || e.typeflag == tar.TypeDir {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Size > 0 && len(e.body) > 0 {
			if _, err := tw.Write(e.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func zipTree(t *testing.T, entries []treeEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		mode := os.FileMode(e.mode) & os.ModePerm
		switch e.typeflag {
		case tar.TypeSymlink:
			mode |= os.ModeSymlink
		case tar.TypeDir:
			mode |= os.ModeDir
			if !strings.HasSuffix(h.Name, "/") {
				h.Name += "/"
			}
		}
		h.SetMode(mode)
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		body := e.body
		if e.typeflag == tar.TypeSymlink {
			body = []byte(e.linkname)
		}
		if len(body) > 0 {
			if _, err := w.Write(body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func newTreeBackend(t *testing.T) *nativeProcessBackend {
	t.Helper()
	raw, err := NewNativeProcessBackend(NativeProcessConfig{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return raw.(*nativeProcessBackend)
}

// TestMaterializeNativeArtifactTree proves the whole tree lands on the host with
// its relative layout preserved and the entrypoint returned + executable — the
// capability nanochat's runner+pyproject+uv.lock archive needs.
func TestMaterializeNativeArtifactTree(t *testing.T) {
	for _, tc := range []struct {
		name    string
		archive []byte
		suffix  string
	}{
		{name: "tar.gz", suffix: ".tar.gz", archive: tarGzipTree(t, []treeEntry{
			{name: "bin/", typeflag: tar.TypeDir, mode: 0o755},
			{name: "bin/runner", typeflag: tar.TypeReg, mode: 0o755, body: []byte("#!runner")},
			{name: "pyproject.toml", typeflag: tar.TypeReg, mode: 0o644, body: []byte("[project]")},
			{name: "uv.lock", typeflag: tar.TypeReg, mode: 0o644, body: []byte("lock")},
		})},
		{name: "zip", suffix: ".zip", archive: zipTree(t, []treeEntry{
			{name: "bin/runner", typeflag: tar.TypeReg, mode: 0o755, body: []byte("#!runner")},
			{name: "pyproject.toml", typeflag: tar.TypeReg, mode: 0o644, body: []byte("[project]")},
			{name: "uv.lock", typeflag: tar.TypeReg, mode: 0o644, body: []byte("lock")},
		})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be := newTreeBackend(t)
			server, sum := artifactServer(t, tc.archive)
			defer server.Close()
			got, err := be.materializeNativeArtifactTree(context.Background(),
				server.URL+"/nanochat"+tc.suffix, sum, "bin/runner")
			if err != nil {
				t.Fatalf("materialize tree: %v", err)
			}
			assertArtifactContents(t, got, []byte("#!runner"))
			// Sibling files the runner needs must sit next to it in the tree.
			treeDir := filepath.Dir(filepath.Dir(got))
			for name, want := range map[string]string{"pyproject.toml": "[project]", "uv.lock": "lock"} {
				b, err := os.ReadFile(filepath.Join(treeDir, name))
				if err != nil || string(b) != want {
					t.Fatalf("sibling %q = %q, %v; want %q", name, b, err, want)
				}
			}

			// Cache hit: the content-addressed tree is reusable with the origin
			// gone, and re-extraction is skipped (same path back).
			server.Close()
			again, err := be.materializeNativeArtifactTree(context.Background(),
				server.URL+"/nanochat"+tc.suffix, sum, "bin/runner")
			if err != nil || again != got {
				t.Fatalf("cached tree = %q, %v; want %q", again, err, got)
			}
		})
	}
}

// TestNativeArtifactTreeContentAddressedByDigest proves the cache key is the
// archive digest alone: two entrypoints selecting into the same archive resolve
// to one shared tree.
func TestNativeArtifactTreeContentAddressedByDigest(t *testing.T) {
	archive := tarGzipTree(t, []treeEntry{
		{name: "bin/a", typeflag: tar.TypeReg, mode: 0o755, body: []byte("a")},
		{name: "bin/b", typeflag: tar.TypeReg, mode: 0o755, body: []byte("b")},
	})
	be := newTreeBackend(t)
	server, sum := artifactServer(t, archive)
	defer server.Close()
	a, err := be.materializeNativeArtifactTree(context.Background(), server.URL+"/t.tar.gz", sum, "bin/a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := be.materializeNativeArtifactTree(context.Background(), server.URL+"/t.tar.gz", sum, "bin/b")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(filepath.Dir(a)) != filepath.Dir(filepath.Dir(b)) {
		t.Fatalf("same archive produced different trees: %q vs %q", a, b)
	}
	assertArtifactContents(t, a, []byte("a"))
	assertArtifactContents(t, b, []byte("b"))
}

// TestNativeArtifactTreeDigestMismatchFailsLoudly — a mismatch is refused and
// never repaired by fetching something else, and leaves no cache entry.
func TestNativeArtifactTreeDigestMismatchFailsLoudly(t *testing.T) {
	archive := tarGzipTree(t, []treeEntry{{name: "bin/runner", typeflag: tar.TypeReg, mode: 0o755, body: []byte("x")}})
	be := newTreeBackend(t)
	server, _ := artifactServer(t, archive)
	defer server.Close()
	_, err := be.materializeNativeArtifactTree(context.Background(),
		server.URL+"/t.tar.gz", fmt.Sprintf("%064x", 1), "bin/runner")
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("digest mismatch error = %v", err)
	}
	assertNoTreeCacheEntry(t, be)
}

// --- MANDATORY: path traversal (tar and zip-slip) ---

func TestNativeArtifactTreeRefusesPathTraversal(t *testing.T) {
	cases := []struct {
		name    string
		member  string
		zipSlip bool
	}{
		{name: "tar parent traversal", member: "../escape"},
		{name: "tar absolute path", member: "/etc/cronjob"},
		{name: "tar nested traversal", member: "bin/../../escape"},
		{name: "zip-slip parent", member: "../escape", zipSlip: true},
		{name: "zip-slip absolute", member: "/etc/cronjob", zipSlip: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			be := newTreeBackend(t)
			entries := []treeEntry{
				{name: "bin/runner", typeflag: tar.TypeReg, mode: 0o755, body: []byte("ok")},
				{name: tc.member, typeflag: tar.TypeReg, mode: 0o644, body: []byte("PWNED")},
			}
			var archive []byte
			suffix := ".tar.gz"
			if tc.zipSlip {
				archive, suffix = zipTree(t, entries), ".zip"
			} else {
				archive = tarGzipTree(t, entries)
			}
			server, sum := artifactServer(t, archive)
			defer server.Close()
			_, err := be.materializeNativeArtifactTree(context.Background(), server.URL+"/t"+suffix, sum, "bin/runner")
			if err == nil || !strings.Contains(err.Error(), "refused") {
				t.Fatalf("traversal member accepted or wrong error: %v", err)
			}
			assertNoTreeCacheEntry(t, be)
		})
	}
}

// --- MANDATORY: symlinks (escape + write-through + hardlink + dangling) ---

func TestNativeArtifactTreeSymlinkDefense(t *testing.T) {
	// A concrete file outside the destination that a naive extractor could be
	// tricked into writing through a symlink.
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("absolute symlink refused (tar)", func(t *testing.T) {
		be := newTreeBackend(t)
		archive := tarGzipTree(t, []treeEntry{
			{name: "bin/runner", typeflag: tar.TypeReg, mode: 0o755, body: []byte("ok")},
			{name: "link", typeflag: tar.TypeSymlink, mode: 0o777, linkname: outside},
		})
		server, sum := artifactServer(t, archive)
		defer server.Close()
		_, err := be.materializeNativeArtifactTree(context.Background(), server.URL+"/t.tar.gz", sum, "bin/runner")
		if err == nil || !strings.Contains(err.Error(), "refused") {
			t.Fatalf("absolute symlink accepted: %v", err)
		}
	})

	t.Run("relative escaping symlink refused (tar)", func(t *testing.T) {
		be := newTreeBackend(t)
		archive := tarGzipTree(t, []treeEntry{
			{name: "bin/runner", typeflag: tar.TypeReg, mode: 0o755, body: []byte("ok")},
			{name: "bin/link", typeflag: tar.TypeSymlink, mode: 0o777, linkname: "../../../../../../etc"},
		})
		server, sum := artifactServer(t, archive)
		defer server.Close()
		_, err := be.materializeNativeArtifactTree(context.Background(), server.URL+"/t.tar.gz", sum, "bin/runner")
		if err == nil || !strings.Contains(err.Error(), "escapes") {
			t.Fatalf("escaping symlink accepted: %v", err)
		}
	})

	// The full write-through sequence: an escaping symlink FOLLOWED by a member
	// writing through it. The escaping link must be refused at creation so the
	// through-write can never reach the outside file.
	for _, tc := range []struct {
		name    string
		zipSlip bool
	}{{name: "tar"}, {name: "zip", zipSlip: true}} {
		t.Run("write-through blocked ("+tc.name+")", func(t *testing.T) {
			if err := os.WriteFile(outside, []byte("original"), 0o600); err != nil {
				t.Fatal(err)
			}
			be := newTreeBackend(t)
			entries := []treeEntry{
				{name: "bin/runner", typeflag: tar.TypeReg, mode: 0o755, body: []byte("ok")},
				{name: "evil", typeflag: tar.TypeSymlink, mode: 0o777, linkname: outside},
				{name: "evil", typeflag: tar.TypeReg, mode: 0o644, body: []byte("PWNED")},
			}
			var archive []byte
			suffix := ".tar.gz"
			if tc.zipSlip {
				archive, suffix = zipTree(t, entries), ".zip"
			} else {
				archive = tarGzipTree(t, entries)
			}
			server, sum := artifactServer(t, archive)
			defer server.Close()
			_, err := be.materializeNativeArtifactTree(context.Background(), server.URL+"/t"+suffix, sum, "bin/runner")
			if err == nil {
				t.Fatal("write-through archive extracted without error")
			}
			if got, _ := os.ReadFile(outside); string(got) != "original" {
				t.Fatalf("outside file was written through symlink: %q", got)
			}
			assertNoTreeCacheEntry(t, be)
		})
	}

	t.Run("hardlink refused (tar)", func(t *testing.T) {
		be := newTreeBackend(t)
		archive := tarGzipTree(t, []treeEntry{
			{name: "bin/runner", typeflag: tar.TypeReg, mode: 0o755, body: []byte("ok")},
			{name: "hl", typeflag: tar.TypeLink, mode: 0o644, linkname: "/etc/shadow"},
		})
		server, sum := artifactServer(t, archive)
		defer server.Close()
		_, err := be.materializeNativeArtifactTree(context.Background(), server.URL+"/t.tar.gz", sum, "bin/runner")
		if err == nil || !strings.Contains(err.Error(), "hardlink") {
			t.Fatalf("hardlink accepted: %v", err)
		}
	})

	t.Run("dangling in-tree symlink allowed (tar)", func(t *testing.T) {
		be := newTreeBackend(t)
		archive := tarGzipTree(t, []treeEntry{
			{name: "bin/runner", typeflag: tar.TypeReg, mode: 0o755, body: []byte("ok")},
			{name: "bin/link", typeflag: tar.TypeSymlink, mode: 0o777, linkname: "does-not-exist-yet"},
		})
		server, sum := artifactServer(t, archive)
		defer server.Close()
		got, err := be.materializeNativeArtifactTree(context.Background(), server.URL+"/t.tar.gz", sum, "bin/runner")
		if err != nil {
			t.Fatalf("harmless in-tree dangling symlink refused: %v", err)
		}
		link := filepath.Join(filepath.Dir(got), "link")
		if _, err := os.Lstat(link); err != nil {
			t.Fatalf("in-tree symlink not created: %v", err)
		}
	})
}

// --- MANDATORY: atomicity + concurrency ---

// A failed extraction must never leave a usable/partial cache entry.
func TestNativeArtifactTreeInterruptedLeavesNoCache(t *testing.T) {
	be := newTreeBackend(t)
	// The second member escapes, so extraction fails after the first file was
	// written into the (private) staging tree — which must never be published.
	archive := tarGzipTree(t, []treeEntry{
		{name: "bin/runner", typeflag: tar.TypeReg, mode: 0o755, body: []byte("ok")},
		{name: "../escape", typeflag: tar.TypeReg, mode: 0o644, body: []byte("PWNED")},
	})
	server, sum := artifactServer(t, archive)
	defer server.Close()
	if _, err := be.materializeNativeArtifactTree(context.Background(), server.URL+"/t.tar.gz", sum, "bin/runner"); err == nil {
		t.Fatal("hostile archive extracted successfully")
	}
	// No published tree, and no partial staging left behind.
	finalDir := filepath.Join(be.dataDir, "artifacts", "tree-"+sum)
	if _, err := os.Stat(finalDir); !os.IsNotExist(err) {
		t.Fatalf("partial tree published at %s (stat err=%v)", finalDir, err)
	}
	assertNoStagingLeftovers(t, be)
}

// Concurrent extraction of the same digest must be safe and converge on one
// tree.
func TestNativeArtifactTreeConcurrentSameDigest(t *testing.T) {
	archive := tarGzipTree(t, []treeEntry{
		{name: "bin/runner", typeflag: tar.TypeReg, mode: 0o755, body: []byte("concurrent")},
	})
	be := newTreeBackend(t)
	server, sum := artifactServer(t, archive)
	defer server.Close()

	const workers = 12
	var wg sync.WaitGroup
	paths := make([]string, workers)
	errs := make([]error, workers)
	start := make(chan struct{})
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			paths[i], errs[i] = be.materializeNativeArtifactTree(
				context.Background(), server.URL+"/t.tar.gz", sum, "bin/runner")
		}(i)
	}
	close(start)
	wg.Wait()
	for i := range workers {
		if errs[i] != nil {
			t.Fatalf("worker %d: %v", i, errs[i])
		}
		if paths[i] != paths[0] {
			t.Fatalf("worker %d path %q != %q", i, paths[i], paths[0])
		}
	}
	assertArtifactContents(t, paths[0], []byte("concurrent"))
}

// --- MANDATORY: decompression bomb + unexpected modes ---

func TestNativeArtifactTreeRefusesHugeDeclaredMember(t *testing.T) {
	be := newTreeBackend(t)
	restore := nativeArtifactTreeMaxFileBytes
	nativeArtifactTreeMaxFileBytes = 512
	defer func() { nativeArtifactTreeMaxFileBytes = restore }()
	// A tar header declaring far more than the per-file cap is refused before a
	// single byte streams, regardless of the (small) body it actually carries.
	archive := tarGzipTree(t, []treeEntry{
		{name: "bin/runner", typeflag: tar.TypeReg, mode: 0o755, body: []byte("ok")},
		{name: "bin/bomb", typeflag: tar.TypeReg, mode: 0o644, body: make([]byte, 1024)},
	})
	server, sum := artifactServer(t, archive)
	defer server.Close()
	_, err := be.materializeNativeArtifactTree(context.Background(), server.URL+"/t.tar.gz", sum, "bin/runner")
	if err == nil || !strings.Contains(err.Error(), "over the") {
		t.Fatalf("oversized member accepted: %v", err)
	}
	assertNoTreeCacheEntry(t, be)
}

func TestNativeArtifactTreeRefusesTotalSizeBomb(t *testing.T) {
	be := newTreeBackend(t)
	restore := nativeArtifactTreeMaxTotalBytes
	nativeArtifactTreeMaxTotalBytes = 16
	defer func() { nativeArtifactTreeMaxTotalBytes = restore }()
	archive := tarGzipTree(t, []treeEntry{
		{name: "bin/runner", typeflag: tar.TypeReg, mode: 0o755, body: bytes.Repeat([]byte("A"), 12)},
		{name: "bin/more", typeflag: tar.TypeReg, mode: 0o644, body: bytes.Repeat([]byte("B"), 12)},
	})
	server, sum := artifactServer(t, archive)
	defer server.Close()
	_, err := be.materializeNativeArtifactTree(context.Background(), server.URL+"/t.tar.gz", sum, "bin/runner")
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("total-size bomb accepted: %v", err)
	}
	assertNoTreeCacheEntry(t, be)
}

func TestNativeArtifactTreeSanitizesFileModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix mode bits")
	}
	be := newTreeBackend(t)
	// setuid + group/other write on a member: never honored.
	archive := tarGzipTree(t, []treeEntry{
		{name: "bin/runner", typeflag: tar.TypeReg, mode: 0o755, body: []byte("ok")},
		{name: "data.txt", typeflag: tar.TypeReg, mode: 0o4777, body: []byte("data")},
	})
	server, sum := artifactServer(t, archive)
	defer server.Close()
	got, err := be.materializeNativeArtifactTree(context.Background(), server.URL+"/t.tar.gz", sum, "bin/runner")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(filepath.Dir(filepath.Dir(got)), "data.txt"))
	if err != nil {
		t.Fatal(err)
	}
	m := info.Mode()
	if m&os.ModeSetuid != 0 {
		t.Fatalf("setuid bit survived extraction: %v", m)
	}
	if m.Perm()&0o077 != 0 {
		t.Fatalf("group/other permission bits survived: %v", m.Perm())
	}
}

// TestResolveCommandTreeAnnotation drives the whole path from Pod annotations,
// the surface nanochat's 2-node design actually uses.
func TestResolveCommandTreeAnnotation(t *testing.T) {
	archive := tarGzipTree(t, []treeEntry{
		{name: "bin/runner", typeflag: tar.TypeReg, mode: 0o755, body: []byte("#!runner")},
		{name: "pyproject.toml", typeflag: tar.TypeReg, mode: 0o644, body: []byte("[project]")},
	})
	be := newTreeBackend(t)
	server, sum := artifactServer(t, archive)
	defer server.Close()
	pod, _ := makeHelperPod(t, "nano-pod", "nano-uid", "runner")
	pod.Annotations = map[string]string{
		NativeArtifactURLAnnotation:    server.URL + "/nanochat.tar.gz",
		NativeArtifactSHA256Annotation: sum,
		NativeArtifactPathAnnotation:   "bin/runner",
		NativeArtifactTreeAnnotation:   "true",
	}
	got, err := be.resolveCommand(context.Background(), pod, "runner")
	if err != nil {
		t.Fatalf("resolveCommand tree: %v", err)
	}
	assertArtifactContents(t, got, []byte("#!runner"))
	if _, err := os.Stat(filepath.Join(filepath.Dir(filepath.Dir(got)), "pyproject.toml")); err != nil {
		t.Fatalf("tree sibling missing after resolveCommand: %v", err)
	}
}

func artifactsRoot(be *nativeProcessBackend) string {
	return filepath.Join(be.dataDir, "artifacts")
}

func assertNoTreeCacheEntry(t *testing.T, be *nativeProcessBackend) {
	t.Helper()
	entries, err := os.ReadDir(artifactsRoot(be))
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "tree-") {
			t.Fatalf("unexpected published tree cache entry: %s", e.Name())
		}
	}
	assertNoStagingLeftovers(t, be)
}

func assertNoStagingLeftovers(t *testing.T, be *nativeProcessBackend) {
	t.Helper()
	entries, err := os.ReadDir(artifactsRoot(be))
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".materialize-") {
			t.Fatalf("staging leftover not cleaned: %s", e.Name())
		}
	}
}
