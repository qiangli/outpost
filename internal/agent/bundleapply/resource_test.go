package bundleapply

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewDynamicClientEmptyPath(t *testing.T) {
	if _, err := NewDynamicClient(""); !errors.Is(err, ErrNoKubeconfig) {
		t.Fatalf("empty kubeconfig must be ErrNoKubeconfig (no default cluster), got %v", err)
	}
	if _, err := NewDynamicClient("   "); !errors.Is(err, ErrNoKubeconfig) {
		t.Fatalf("blank kubeconfig must be ErrNoKubeconfig, got %v", err)
	}
}

func TestNewDynamicClientMissingFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nope.yaml")
	_, err := NewDynamicClient(p)
	if !errors.Is(err, ErrNoKubeconfig) {
		t.Fatalf("missing kubeconfig file must be ErrNoKubeconfig (no silent fallback), got %v", err)
	}
}

func TestExpandUser(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}
	got, err := ExpandUser("~/.kube/outpost-control-plane/k3s.yaml")
	if err != nil {
		t.Fatalf("ExpandUser: %v", err)
	}
	want := filepath.Join(home, ".kube/outpost-control-plane/k3s.yaml")
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
	// A non-tilde path is returned unchanged.
	if got, _ := ExpandUser("/abs/path"); got != "/abs/path" {
		t.Fatalf("absolute path must pass through, got %q", got)
	}
}

// The two conventional kubeconfig paths must never be equal — conflating
// them is the exact bug this package is built to avoid.
func TestPeerAndCloudboxPathsDistinct(t *testing.T) {
	if PeerControlPlaneKubeconfig == CloudboxKubeconfig {
		t.Fatal("peer and cloudbox kubeconfig paths must differ")
	}
	if !strings.Contains(PeerControlPlaneKubeconfig, "outpost-control-plane") {
		t.Fatalf("peer path lost its control-plane marker: %q", PeerControlPlaneKubeconfig)
	}
}
