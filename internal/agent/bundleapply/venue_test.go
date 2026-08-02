package bundleapply

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture: a fake home layout with a peer kubeconfig, a cloudbox
// kubeconfig, and various sneaky routes to the latter.
type venueFix struct {
	dir      string
	peer     string // <dir>/kube/outpost-control-plane/k3s.yaml
	cloudbox string // <dir>/kube/outpost.yaml
}

func newVenueFix(t *testing.T) venueFix {
	t.Helper()
	dir := t.TempDir()
	// Canonicalize the temp dir itself: on macOS t.TempDir() lives under
	// /var → /private/var, and the tests compare resolved paths.
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	peerDir := filepath.Join(dir, "kube", "outpost-control-plane")
	if err := os.MkdirAll(peerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	f := venueFix{
		dir:      dir,
		peer:     filepath.Join(peerDir, "k3s.yaml"),
		cloudbox: filepath.Join(dir, "kube", "outpost.yaml"),
	}
	for _, p := range []string{f.peer, f.cloudbox} {
		if err := os.WriteFile(p, []byte("apiVersion: v1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func TestResolveVenueAcceptsPeerPath(t *testing.T) {
	f := newVenueFix(t)
	got, err := resolveVenueAgainst(f.peer, f.cloudbox)
	if err != nil {
		t.Fatalf("peer path must resolve: %v", err)
	}
	if got != f.peer {
		t.Fatalf("want %q, got %q", f.peer, got)
	}
}

func TestResolveVenueRejectsCloudboxDirect(t *testing.T) {
	f := newVenueFix(t)
	if _, err := resolveVenueAgainst(f.cloudbox, f.cloudbox); !errors.Is(err, ErrCloudboxVenue) {
		t.Fatalf("direct cloudbox path must be refused, got %v", err)
	}
}

// The core of requirement 1: a SYMLINK to the cloudbox kubeconfig — or a
// symlinked directory containing it — must still be recognized and refused.
func TestResolveVenueRejectsSymlinkToCloudbox(t *testing.T) {
	f := newVenueFix(t)

	link := filepath.Join(f.dir, "innocent-looking.yaml")
	if err := os.Symlink(f.cloudbox, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := resolveVenueAgainst(link, f.cloudbox); !errors.Is(err, ErrCloudboxVenue) {
		t.Fatalf("file symlink to cloudbox must be refused, got %v", err)
	}

	// Directory symlink: <dir>/aliased-kube → <dir>/kube, then reference
	// the cloudbox file through the alias.
	dirLink := filepath.Join(f.dir, "aliased-kube")
	if err := os.Symlink(filepath.Join(f.dir, "kube"), dirLink); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := resolveVenueAgainst(filepath.Join(dirLink, "outpost.yaml"), f.cloudbox); !errors.Is(err, ErrCloudboxVenue) {
		t.Fatal("cloudbox reached through a symlinked directory must be refused")
	}
}

// `..` traversal that lands on the cloudbox file is refused after
// canonicalization.
func TestResolveVenueRejectsDotDotTraversalToCloudbox(t *testing.T) {
	f := newVenueFix(t)
	sneaky := filepath.Join(f.dir, "kube", "outpost-control-plane", "..", "outpost.yaml")
	if _, err := resolveVenueAgainst(sneaky, f.cloudbox); !errors.Is(err, ErrCloudboxVenue) {
		t.Fatalf("dot-dot traversal to cloudbox must be refused, got %v", err)
	}
}

// A relative path that reaches the cloudbox file is refused too.
func TestResolveVenueRejectsRelativeRouteToCloudbox(t *testing.T) {
	f := newVenueFix(t)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(filepath.Join(f.dir, "kube")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if _, err := resolveVenueAgainst("outpost.yaml", f.cloudbox); !errors.Is(err, ErrCloudboxVenue) {
		t.Fatalf("relative route to cloudbox must be refused, got %v", err)
	}
}

// A path that cannot be canonicalized FAILS — it is not "probably fine".
func TestResolveVenueUnresolvableFails(t *testing.T) {
	f := newVenueFix(t)

	// Missing file.
	if _, err := resolveVenueAgainst(filepath.Join(f.dir, "nope.yaml"), f.cloudbox); !errors.Is(err, ErrVenueUnresolvable) {
		t.Fatalf("missing venue must be ErrVenueUnresolvable, got %v", err)
	}

	// Dangling symlink.
	dangling := filepath.Join(f.dir, "dangling.yaml")
	if err := os.Symlink(filepath.Join(f.dir, "gone.yaml"), dangling); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := resolveVenueAgainst(dangling, f.cloudbox); !errors.Is(err, ErrVenueUnresolvable) {
		t.Fatalf("dangling symlink must be ErrVenueUnresolvable, got %v", err)
	}
}

// The guard also holds when the cloudbox REFERENCE itself does not exist
// (best-effort canonicalization of the reference side): a symlinked route
// spelled differently but landing on the same missing path is refused.
func TestResolveVenueCloudboxReferenceNeedNotExist(t *testing.T) {
	f := newVenueFix(t)
	missingCloudbox := filepath.Join(f.dir, "kube", "not-written-yet.yaml")
	// Target: an EXISTING file that is not the cloudbox one — accepted.
	if _, err := resolveVenueAgainst(f.peer, missingCloudbox); err != nil {
		t.Fatalf("peer path must resolve against a missing cloudbox reference: %v", err)
	}
	// Target spelled via .. but landing on the missing reference path
	// can't exist (EvalSymlinks fails) — unresolvable, which still FAILS.
	sneaky := filepath.Join(f.dir, "kube", "outpost-control-plane", "..", "not-written-yet.yaml")
	if _, err := resolveVenueAgainst(sneaky, missingCloudbox); err == nil {
		t.Fatal("nonexistent target must fail")
	}
}

// NewDynamicClient — the production constructor — enforces the guard
// itself, so a Go caller bypassing the script cannot reach the cloudbox
// plane. (The guard fires before any network dial, so this is fully
// offline.)
func TestNewDynamicClientEnforcesVenueGuard(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	kubeDir := filepath.Join(home, ".kube")
	if err := os.MkdirAll(kubeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cloudbox := filepath.Join(kubeDir, "outpost.yaml")
	if err := os.WriteFile(cloudbox, []byte("apiVersion: v1\nkind: Config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewDynamicClient(cloudbox); !errors.Is(err, ErrCloudboxVenue) {
		t.Fatalf("NewDynamicClient must refuse the cloudbox kubeconfig, got %v", err)
	}
	// And via a symlink.
	link := filepath.Join(home, "sneaky.yaml")
	if err := os.Symlink(cloudbox, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := NewDynamicClient(link); !errors.Is(err, ErrCloudboxVenue) {
		t.Fatalf("NewDynamicClient must refuse a symlink to the cloudbox kubeconfig, got %v", err)
	}
}

func TestCanonicalizePathExpandsTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	target := filepath.Join(home, "cfg.yaml")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := CanonicalizePath("~/cfg.yaml")
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(target)
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
	if !strings.HasPrefix(got, string(filepath.Separator)) {
		t.Fatalf("canonical path must be absolute, got %q", got)
	}
}
