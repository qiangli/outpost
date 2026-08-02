package vknode

// GATE-ADDED TESTS (sprint 42, outpost#39 review).
//
// These probe the symlink-write-through defense with a CHAINED link, i.e. a
// symlink whose target traverses a previously-created in-tree symlink. The
// existing tests only cover DIRECT escapes (absolute linkname, "../../.."),
// which the lexical filepath.Join check in writeNativeArtifactTreeSymlink does
// catch. It does not resolve symlink components, so a lexically-neutral
// linkname can be kernel-wise escaping.

import (
	"archive/tar"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A symlink "sub/link" -> ".." is in-tree and allowed (destDir/sub/.. == destDir).
// A second symlink "esc" -> "sub/link/../.." is LEXICALLY neutral (Clean cancels
// "sub","link" against the two "..") so it passes the escape check, but the
// KERNEL resolves sub/link to destDir first, so the two ".." climb two levels
// ABOVE destDir. A following member written "through" esc then lands outside the
// artifact tree.
func TestGateChainedSymlinkWriteThroughEscapes(t *testing.T) {
	be := newTreeBackend(t)

	archive := tarGzipTree(t, []treeEntry{
		{name: "bin/runner", typeflag: tar.TypeReg, mode: 0o755, body: []byte("ok")},
		{name: "sub", typeflag: tar.TypeDir, mode: 0o755},
		// in-tree, allowed by the current check
		{name: "sub/link", typeflag: tar.TypeSymlink, mode: 0o777, linkname: ".."},
		// lexically neutral, kernel-wise climbs 2 levels out of destDir
		{name: "esc", typeflag: tar.TypeSymlink, mode: 0o777, linkname: "sub/link/../.."},
		// write through it
		{name: "esc/PWNED", typeflag: tar.TypeReg, mode: 0o644, body: []byte("PWNED")},
	})
	server, sum := artifactServer(t, archive)
	defer server.Close()

	entry, err := be.materializeNativeArtifactTree(
		context.Background(), server.URL+"/t.tar.gz", sum, "bin/runner")
	t.Logf("materialize err=%v entry=%q", err, entry)

	// cacheRoot == <dataDir>/artifacts. destDir is <cacheRoot>/.materialize-*/tree,
	// so a 2-level climb lands the write directly in cacheRoot.
	cacheRoot := filepath.Join(be.dataDir, "artifacts")
	escaped := filepath.Join(cacheRoot, "PWNED")
	if body, rerr := os.ReadFile(escaped); rerr == nil {
		t.Fatalf("SECURITY: member written OUTSIDE the artifact tree at %s (content %q)", escaped, body)
	}
	if err == nil {
		t.Errorf("chained escaping symlink archive extracted without error")
	}
}

// The same defect, escalated: enough hops climb past the filesystem root, so the
// through-write reaches an ABSOLUTE path of the attacker's choosing. This is the
// arbitrary-file-write / RCE form. The victim path here is a test-owned temp dir.
func TestGateChainedSymlinkReachesAbsolutePath(t *testing.T) {
	be := newTreeBackend(t)

	victimDir := t.TempDir()
	victim := filepath.Join(victimDir, "PWNED")

	// 14 hops => kernel climbs 28 levels (clamped at "/"), lexically neutral.
	// Stays under the kernel's MAXSYMLINKS (~32) per-resolution limit.
	const hops = 14
	link := strings.Repeat("sub/link/", hops) + strings.TrimSuffix(strings.Repeat("../", hops*2), "/")

	// member path relative to esc, which resolves to "/"
	rel := strings.TrimPrefix(filepath.ToSlash(victim), "/")

	archive := tarGzipTree(t, []treeEntry{
		{name: "bin/runner", typeflag: tar.TypeReg, mode: 0o755, body: []byte("ok")},
		{name: "sub", typeflag: tar.TypeDir, mode: 0o755},
		{name: "sub/link", typeflag: tar.TypeSymlink, mode: 0o777, linkname: ".."},
		{name: "esc", typeflag: tar.TypeSymlink, mode: 0o777, linkname: link},
		{name: "esc/" + rel, typeflag: tar.TypeReg, mode: 0o644, body: []byte("PWNED")},
	})
	server, sum := artifactServer(t, archive)
	defer server.Close()

	_, err := be.materializeNativeArtifactTree(
		context.Background(), server.URL+"/t.tar.gz", sum, "bin/runner")
	t.Logf("materialize err=%v", err)

	if body, rerr := os.ReadFile(victim); rerr == nil {
		t.Fatalf("SECURITY: arbitrary absolute-path write to %s (content %q)", victim, body)
	}
	if err == nil {
		t.Errorf("chained escaping symlink archive extracted without error")
	}
}

// Zip is a distinct code path; the same chain must be refused there too.
func TestGateChainedSymlinkWriteThroughEscapesZip(t *testing.T) {
	be := newTreeBackend(t)

	archive := zipTree(t, []treeEntry{
		{name: "bin/runner", typeflag: tar.TypeReg, mode: 0o755, body: []byte("ok")},
		{name: "sub", typeflag: tar.TypeDir, mode: 0o755},
		{name: "sub/link", typeflag: tar.TypeSymlink, mode: 0o777, linkname: ".."},
		{name: "esc", typeflag: tar.TypeSymlink, mode: 0o777, linkname: "sub/link/../.."},
		{name: "esc/PWNED", typeflag: tar.TypeReg, mode: 0o644, body: []byte("PWNED")},
	})
	server, sum := artifactServer(t, archive)
	defer server.Close()

	_, err := be.materializeNativeArtifactTree(
		context.Background(), server.URL+"/t.zip", sum, "bin/runner")
	t.Logf("materialize err=%v", err)

	cacheRoot := filepath.Join(be.dataDir, "artifacts")
	escaped := filepath.Join(cacheRoot, "PWNED")
	if body, rerr := os.ReadFile(escaped); rerr == nil {
		t.Fatalf("SECURITY: zip member written OUTSIDE the artifact tree at %s (content %q)", escaped, body)
	}
	if err == nil {
		t.Errorf("chained escaping symlink zip extracted without error")
	}
}

// Even without a second symlink: a DIRECTORY member whose path traverses an
// escaping chained link creates directories outside the tree.
func TestGateChainedSymlinkDirCreationEscapes(t *testing.T) {
	be := newTreeBackend(t)

	archive := tarGzipTree(t, []treeEntry{
		{name: "bin/runner", typeflag: tar.TypeReg, mode: 0o755, body: []byte("ok")},
		{name: "sub", typeflag: tar.TypeDir, mode: 0o755},
		{name: "sub/link", typeflag: tar.TypeSymlink, mode: 0o777, linkname: ".."},
		{name: "esc", typeflag: tar.TypeSymlink, mode: 0o777, linkname: "sub/link/../.."},
		{name: "esc/PWNEDDIR", typeflag: tar.TypeDir, mode: 0o755},
	})
	server, sum := artifactServer(t, archive)
	defer server.Close()

	_, err := be.materializeNativeArtifactTree(
		context.Background(), server.URL+"/t.tar.gz", sum, "bin/runner")
	t.Logf("materialize err=%v", err)

	cacheRoot := filepath.Join(be.dataDir, "artifacts")
	escaped := filepath.Join(cacheRoot, "PWNEDDIR")
	if st, serr := os.Lstat(escaped); serr == nil && st.IsDir() {
		t.Fatalf("SECURITY: directory created OUTSIDE the artifact tree at %s", escaped)
	}
}
