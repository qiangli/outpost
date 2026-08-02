package vknode

// Sprint 42: extraction is mediated by an *os.Root opened on the tree dir, so
// containment is a property of kernel path resolution rather than of a lexical
// path check. These tests probe the cases a lexical check structurally cannot
// see (link chains that clean to a neutral path), the cases it would wrongly
// reject (legitimate in-tree links resolved against members that appear later),
// and the link/dir collisions between members.

import (
	"archive/tar"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// deepChainEntries builds a THREE-hop symlink chain that is lexically neutral at
// every step, so nothing here is refused by the fail-fast checks in
// writeNativeArtifactTreeSymlink:
//
//	d1/l -> ".."          cleans to "."      (d1 cancels)
//	d2/l -> "../d1/l"     cleans to "d1/l"
//	d3/l -> "../d2/l"     cleans to "d2/l"
//	esc  -> "d3/l/../.."  cleans to "."      <- NEUTRAL, but the kernel resolves
//	                                            d3/l -> d2/l -> d1/l -> treeDir,
//	                                            so ".." twice lands two levels
//	                                            ABOVE the tree.
//
// Four links is far below the kernel's MAXSYMLINKS, so a refusal here is real
// containment and not an incidental ELOOP.
func deepChainEntries(member treeEntry) []treeEntry {
	return []treeEntry{
		{name: "bin/runner", typeflag: tar.TypeReg, mode: 0o755, body: []byte("ok")},
		{name: "d1", typeflag: tar.TypeDir, mode: 0o755},
		{name: "d1/l", typeflag: tar.TypeSymlink, mode: 0o777, linkname: ".."},
		{name: "d2", typeflag: tar.TypeDir, mode: 0o755},
		{name: "d2/l", typeflag: tar.TypeSymlink, mode: 0o777, linkname: "../d1/l"},
		{name: "d3", typeflag: tar.TypeDir, mode: 0o755},
		{name: "d3/l", typeflag: tar.TypeSymlink, mode: 0o777, linkname: "../d2/l"},
		{name: "esc", typeflag: tar.TypeSymlink, mode: 0o777, linkname: "d3/l/../.."},
		member,
	}
}

var (
	escFileMember = treeEntry{name: "esc/DEEP/PWNED", typeflag: tar.TypeReg, mode: 0o644, body: []byte("PWNED")}
	escDirMember  = treeEntry{name: "esc/DEEP", typeflag: tar.TypeDir, mode: 0o755}
)

func TestNativeArtifactTreeRefusesDeepSymlinkChain(t *testing.T) {
	for _, tc := range []struct {
		name    string
		member  treeEntry
		zipSlip bool
	}{
		{name: "tar file through chain", member: escFileMember},
		{name: "tar dir through chain", member: escDirMember},
		{name: "zip file through chain", member: escFileMember, zipSlip: true},
		{name: "zip dir through chain", member: escDirMember, zipSlip: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be := newTreeBackend(t)
			entries := deepChainEntries(tc.member)
			archive, suffix := tarGzipTree(t, entries), ".tar.gz"
			if tc.zipSlip {
				archive, suffix = zipTree(t, entries), ".zip"
			}
			server, sum := artifactServer(t, archive)
			defer server.Close()

			_, err := be.materializeNativeArtifactTree(
				context.Background(), server.URL+"/t"+suffix, sum, "bin/runner")
			t.Logf("materialize err=%v", err)
			if err == nil {
				t.Fatal("deep chained escaping symlink extracted without error")
			}
			// A symlink-loop error would mean the kernel ran out of hops, not that
			// the tree was contained. Containment must be what stops this.
			if strings.Contains(err.Error(), "too many levels of symbolic links") {
				t.Fatalf("blocked by ELOOP rather than by root containment: %v", err)
			}

			// The chain climbs two levels above the tree: <cacheRoot>/.materialize-*/tree
			// so the write would land in cacheRoot.
			cacheRoot := artifactsRoot(be)
			for _, escaped := range []string{
				filepath.Join(cacheRoot, "DEEP"),
				filepath.Join(cacheRoot, "DEEP", "PWNED"),
			} {
				if _, serr := os.Lstat(escaped); serr == nil {
					t.Fatalf("SECURITY: %s created OUTSIDE the artifact tree", escaped)
				}
			}
			assertNoTreeCacheEntry(t, be)
		})
	}
}

// The mirror of the escape tests: a link whose target does not exist YET must
// still resolve once the archive creates it, and a member written through that
// link must land inside the tree. Refusing this would break legitimate archives
// (a bin/ symlink into a versioned payload dir declared later in the tar).
func TestNativeArtifactTreeAllowsLinkTargetCreatedLater(t *testing.T) {
	be := newTreeBackend(t)
	archive := tarGzipTree(t, []treeEntry{
		{name: "bin/runner", typeflag: tar.TypeReg, mode: 0o755, body: []byte("ok")},
		// dangling at creation time — "later" appears further down the archive
		{name: "link", typeflag: tar.TypeSymlink, mode: 0o777, linkname: "later"},
		{name: "later", typeflag: tar.TypeDir, mode: 0o755},
		// written THROUGH the now-resolvable link; lands at later/data
		{name: "link/data", typeflag: tar.TypeReg, mode: 0o644, body: []byte("payload")},
	})
	server, sum := artifactServer(t, archive)
	defer server.Close()

	entry, err := be.materializeNativeArtifactTree(
		context.Background(), server.URL+"/t.tar.gz", sum, "bin/runner")
	if err != nil {
		t.Fatalf("legitimate in-tree link resolved against a later member was refused: %v", err)
	}
	treeDir := filepath.Dir(filepath.Dir(entry))
	got, err := os.ReadFile(filepath.Join(treeDir, "later", "data"))
	if err != nil {
		t.Fatalf("member written through the in-tree link is missing: %v", err)
	}
	if string(got) != "payload" {
		t.Fatalf("member content = %q, want %q", got, "payload")
	}
}

// TOCTOU between members: a later member must not be able to replace an
// already-extracted path with a symlink and so re-point everything under it.
func TestNativeArtifactTreeRefusesMemberReplacedBySymlink(t *testing.T) {
	for _, tc := range []struct {
		name    string
		victim  treeEntry
		zipSlip bool
	}{
		{name: "dir replaced by symlink (tar)", victim: treeEntry{name: "sub", typeflag: tar.TypeDir, mode: 0o755}},
		{name: "file replaced by symlink (tar)", victim: treeEntry{name: "sub", typeflag: tar.TypeReg, mode: 0o644, body: []byte("benign")}},
		{name: "dir replaced by symlink (zip)", victim: treeEntry{name: "sub", typeflag: tar.TypeDir, mode: 0o755}, zipSlip: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be := newTreeBackend(t)
			entries := []treeEntry{
				{name: "bin/runner", typeflag: tar.TypeReg, mode: 0o755, body: []byte("ok")},
				tc.victim,
				// An in-tree linkname, so the fail-fast lexical checks pass and the
				// collision itself has to be what refuses this.
				{name: "sub", typeflag: tar.TypeSymlink, mode: 0o777, linkname: "elsewhere"},
			}
			archive, suffix := tarGzipTree(t, entries), ".tar.gz"
			if tc.zipSlip {
				archive, suffix = zipTree(t, entries), ".zip"
			}
			server, sum := artifactServer(t, archive)
			defer server.Close()

			_, err := be.materializeNativeArtifactTree(
				context.Background(), server.URL+"/t"+suffix, sum, "bin/runner")
			if err == nil {
				t.Fatal("member replacing an extracted path with a symlink was accepted")
			}
			t.Logf("materialize err=%v", err)
			assertNoTreeCacheEntry(t, be)
		})
	}
}

// Hardlinks are refused unconditionally — including one naming an in-tree
// member, so the refusal cannot be mistaken for a target-validation rule that a
// cleverer linkname could slip past.
func TestNativeArtifactTreeRefusesHardlinkRegardlessOfTarget(t *testing.T) {
	for _, linkname := range []string{"bin/runner", "../../../etc/passwd", "/etc/shadow"} {
		t.Run(linkname, func(t *testing.T) {
			be := newTreeBackend(t)
			archive := tarGzipTree(t, []treeEntry{
				{name: "bin/runner", typeflag: tar.TypeReg, mode: 0o755, body: []byte("ok")},
				{name: "hl", typeflag: tar.TypeLink, mode: 0o644, linkname: linkname},
			})
			server, sum := artifactServer(t, archive)
			defer server.Close()

			_, err := be.materializeNativeArtifactTree(
				context.Background(), server.URL+"/t.tar.gz", sum, "bin/runner")
			if err == nil || !strings.Contains(err.Error(), "hardlink") {
				t.Fatalf("hardlink to %q accepted or wrong error: %v", linkname, err)
			}
			assertNoTreeCacheEntry(t, be)
		})
	}
}

// The entrypoint's 0700 is applied to the open descriptor rather than by path.
// Assert the published entrypoint really is owner-executable, since a silently
// failed fd-chmod would otherwise surface only as an exec failure at launch.
func TestNativeArtifactTreeEntrypointIsExecutable(t *testing.T) {
	be := newTreeBackend(t)
	archive := tarGzipTree(t, []treeEntry{
		// Deliberately NOT marked executable in the archive: the entrypoint chmod,
		// not the member's own mode, is what has to make this runnable.
		{name: "bin/runner", typeflag: tar.TypeReg, mode: 0o644, body: []byte("#!runner")},
	})
	server, sum := artifactServer(t, archive)
	defer server.Close()

	got, err := be.materializeNativeArtifactTree(
		context.Background(), server.URL+"/t.tar.gz", sum, "bin/runner")
	if err != nil {
		t.Fatalf("materialize tree: %v", err)
	}
	info, err := os.Lstat(got)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("entrypoint mode = %v, want 0700", info.Mode().Perm())
	}
}

// The entrypoint is Lstat'ed, not Stat'ed: a symlink entrypoint is refused
// instead of being followed, chmod'ed 0700, and executed. With Stat this passes
// as a "regular file" and the mode change lands on the link's target.
func TestNativeArtifactTreeRefusesSymlinkEntrypoint(t *testing.T) {
	be := newTreeBackend(t)
	archive := tarGzipTree(t, []treeEntry{
		{name: "bin/target", typeflag: tar.TypeReg, mode: 0o644, body: []byte("ok")},
		{name: "bin/runner", typeflag: tar.TypeSymlink, mode: 0o777, linkname: "target"},
	})
	server, sum := artifactServer(t, archive)
	defer server.Close()

	_, err := be.materializeNativeArtifactTree(
		context.Background(), server.URL+"/t.tar.gz", sum, "bin/runner")
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("symlink entrypoint accepted or wrong error: %v", err)
	}
	assertNoTreeCacheEntry(t, be)
}
