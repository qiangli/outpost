package admincore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/qiangli/outpost/internal/agent/bundleapply"
	"github.com/qiangli/outpost/internal/agent/conf"
)

// fakeBundleClient is a deterministic in-memory bundleapply.ResourceClient
// for the admincore surface tests — no apiserver, no network.
type fakeBundleClient struct {
	mu     sync.Mutex
	store  map[string]*unstructured.Unstructured
	closed bool
}

func newFakeBundleClient() *fakeBundleClient {
	return &fakeBundleClient{store: map[string]*unstructured.Unstructured{}}
}

func bkey(o *unstructured.Unstructured) string {
	return o.GetKind() + "|" + o.GetNamespace() + "|" + o.GetName()
}

func (f *fakeBundleClient) Apply(_ context.Context, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.store[bkey(obj)] = obj.DeepCopy()
	return obj, nil
}

func (f *fakeBundleClient) Get(_ context.Context, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	live, ok := f.store[bkey(obj)]
	if !ok {
		return nil, fmt.Errorf("fake: %s: %w", bkey(obj), bundleapply.ErrNotFound)
	}
	return live.DeepCopy(), nil
}

func (f *fakeBundleClient) Delete(_ context.Context, obj *unstructured.Unstructured) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.store, bkey(obj))
	return nil
}

func (f *fakeBundleClient) ServesGVK(_ context.Context, _ schema.GroupVersionKind) (bool, error) {
	return true, nil
}

// bundleFixture prepares: an admincore.Server whose HOME is a temp dir, a
// peer kubeconfig file, the cloudbox kubeconfig file at its conventional
// ~/.kube/outpost.yaml, a trivial bundle dir, and a recording client
// factory. The factory records the (already canonicalized, venue-checked)
// kubeconfig admincore hands it.
type bundleFixture struct {
	srv        *Server
	home       string
	peer       string
	cloudbox   string
	bundleDir  string
	got        []string // kubeconfig paths the factory received
	fakeClient *fakeBundleClient
}

func newBundleFixture(t *testing.T) *bundleFixture {
	t.Helper()
	fix := &bundleFixture{fakeClient: newFakeBundleClient()}

	tmp := t.TempDir()
	// Resolve the temp dir (macOS /var → /private/var) so path equality
	// assertions compare canonical forms.
	tmp, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		t.Fatal(err)
	}
	fix.home = tmp
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("XDG_CACHE_HOME", tmp)
	t.Setenv("HOME", tmp)

	peerDir := filepath.Join(tmp, ".kube", "outpost-control-plane")
	if err := os.MkdirAll(peerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fix.peer = filepath.Join(peerDir, "k3s.yaml")
	fix.cloudbox = filepath.Join(tmp, ".kube", "outpost.yaml")
	for _, p := range []string{fix.peer, fix.cloudbox} {
		if err := os.WriteFile(p, []byte("apiVersion: v1\nkind: Config\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	fix.bundleDir = filepath.Join(tmp, "bundle")
	if err := os.MkdirAll(fix.bundleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: demo\n---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cfg\n  namespace: demo\n"
	if err := os.WriteFile(filepath.Join(fix.bundleDir, "all.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}

	srv, err := New(Deps{
		ConfigPath: filepath.Join(tmp, "agent.json"),
		BundleApplyClient: func(kubeconfig string) (bundleapply.ResourceClient, error) {
			fix.got = append(fix.got, kubeconfig)
			return fix.fakeClient, nil
		},
	})
	if err != nil {
		t.Fatalf("admincore.New: %v", err)
	}
	fix.srv = srv
	return fix
}

func TestBundleApply_RequiresBundlePath(t *testing.T) {
	fix := newBundleFixture(t)
	_, err := fix.srv.BundleApply(context.Background(), BundleApplyParams{Kubeconfig: fix.peer})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 400 {
		t.Fatalf("missing bundle must be a 400, got %v", err)
	}
}

// The venue guard fires INSIDE admincore (Go API) — even though the test
// injects a fake client factory, the cloudbox kubeconfig is refused
// before the factory is ever consulted. Symlinked routes included.
func TestBundleApply_VenueGuardRefusesCloudbox(t *testing.T) {
	fix := newBundleFixture(t)

	_, err := fix.srv.BundleApply(context.Background(), BundleApplyParams{
		Kubeconfig: fix.cloudbox, Bundle: fix.bundleDir,
	})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 400 || !strings.Contains(apiErr.Msg, "cloudbox") {
		t.Fatalf("cloudbox venue must be refused with a 400 naming the conflation, got %v", err)
	}

	link := filepath.Join(fix.home, "sneaky.yaml")
	if err := os.Symlink(fix.cloudbox, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	_, err = fix.srv.BundleApply(context.Background(), BundleApplyParams{
		Kubeconfig: link, Bundle: fix.bundleDir,
	})
	if !errors.As(err, &apiErr) || !strings.Contains(apiErr.Msg, "cloudbox") {
		t.Fatalf("symlink to cloudbox must be refused, got %v", err)
	}
	if len(fix.got) != 0 {
		t.Fatalf("client factory must never see a refused venue, got %v", fix.got)
	}
}

// Venue resolution order: explicit param > persisted
// cluster.bundle_kubeconfig > conventional peer default. And an
// unresolvable venue (missing default) FAILS rather than passing.
func TestBundleApply_VenueResolutionOrder(t *testing.T) {
	fix := newBundleFixture(t)

	// Nothing persisted, no param: the conventional peer default — which
	// exists in this fixture — is used.
	res, err := fix.srv.BundleApply(context.Background(), BundleApplyParams{Bundle: fix.bundleDir})
	if err != nil {
		t.Fatalf("default venue apply: %v", err)
	}
	if res.Kubeconfig != fix.peer {
		t.Fatalf("default venue must be the conventional peer path %q, got %q", fix.peer, res.Kubeconfig)
	}

	// Persisted key wins over the default.
	alt := filepath.Join(fix.home, "alt-k3s.yaml")
	if err := os.WriteFile(alt, []byte("apiVersion: v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := conf.SaveFile(filepath.Join(fix.home, "agent.json"), &conf.FileConfig{
		Cluster: &conf.ClusterConfig{BundleKubeconfig: alt},
	}); err != nil {
		t.Fatal(err)
	}
	res, err = fix.srv.BundleApply(context.Background(), BundleApplyParams{Bundle: fix.bundleDir})
	if err != nil {
		t.Fatalf("persisted venue apply: %v", err)
	}
	if res.Kubeconfig != alt {
		t.Fatalf("persisted bundle_kubeconfig must win over the default, got %q", res.Kubeconfig)
	}

	// Explicit param wins over the persisted key.
	res, err = fix.srv.BundleApply(context.Background(), BundleApplyParams{Bundle: fix.bundleDir, Kubeconfig: fix.peer})
	if err != nil {
		t.Fatalf("explicit venue apply: %v", err)
	}
	if res.Kubeconfig != fix.peer {
		t.Fatalf("explicit kubeconfig must win, got %q", res.Kubeconfig)
	}

	// A venue that cannot be canonicalized fails loudly.
	_, err = fix.srv.BundleApply(context.Background(), BundleApplyParams{
		Bundle: fix.bundleDir, Kubeconfig: filepath.Join(fix.home, "missing.yaml"),
	})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 400 {
		t.Fatalf("unresolvable venue must be a 400, got %v", err)
	}
}

func TestBundleApply_AppliesAndReports(t *testing.T) {
	fix := newBundleFixture(t)
	res, err := fix.srv.BundleApply(context.Background(), BundleApplyParams{
		Bundle: fix.bundleDir, Kubeconfig: fix.peer, TimeoutSeconds: 5,
	})
	if err != nil {
		t.Fatalf("BundleApply: %v", err)
	}
	if !res.OK || res.Applied != 2 || res.Ready != 2 || len(res.Created) != 2 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if len(fix.got) != 1 || fix.got[0] != fix.peer {
		t.Fatalf("factory must receive the canonical venue once, got %v", fix.got)
	}
}

// save_kubeconfig persists the venue as cluster.bundle_kubeconfig — but
// only after a SUCCESSFUL apply.
func TestBundleApply_SaveKubeconfig(t *testing.T) {
	fix := newBundleFixture(t)
	configPath := filepath.Join(fix.home, "agent.json")

	// Saving without an explicit kubeconfig is a category error.
	_, err := fix.srv.BundleApply(context.Background(), BundleApplyParams{
		Bundle: fix.bundleDir, SaveKubeconfig: true,
	})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 400 {
		t.Fatalf("save without explicit kubeconfig must be 400, got %v", err)
	}

	// A failing apply must NOT persist the venue.
	failing := filepath.Join(fix.bundleDir, "..", "failing")
	if err := os.MkdirAll(failing, 0o755); err != nil {
		t.Fatal(err)
	}
	dep := "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\n  namespace: demo\nspec:\n  replicas: 1\n"
	if err := os.WriteFile(filepath.Join(failing, "dep.yaml"), []byte(dep), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = fix.srv.BundleApply(context.Background(), BundleApplyParams{
		Bundle: failing, Kubeconfig: fix.peer, SaveKubeconfig: true, TimeoutSeconds: 1, PollSeconds: 1,
	})
	if err == nil {
		t.Fatal("never-ready deployment must fail")
	}
	fc, err := conf.LoadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	// LoadFile returns nil for a never-written config — which also proves
	// nothing was persisted.
	if fc != nil && fc.Cluster != nil && fc.Cluster.BundleKubeconfig != "" {
		t.Fatalf("failed apply must not persist bundle_kubeconfig, got %q", fc.Cluster.BundleKubeconfig)
	}

	// A successful apply persists it, and the view reports it.
	res, err := fix.srv.BundleApply(context.Background(), BundleApplyParams{
		Bundle: fix.bundleDir, Kubeconfig: fix.peer, SaveKubeconfig: true, TimeoutSeconds: 5,
	})
	if err != nil || !res.KubeconfigSaved {
		t.Fatalf("successful save-apply: res=%+v err=%v", res, err)
	}
	fc, err = conf.LoadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if fc.Cluster == nil || fc.Cluster.BundleKubeconfig != fix.peer {
		t.Fatalf("bundle_kubeconfig not persisted, got %+v", fc.Cluster)
	}
	view, err := fix.srv.BundleKubeconfig()
	if err != nil || view.Kubeconfig != fix.peer {
		t.Fatalf("BundleKubeconfig view: %+v err=%v", view, err)
	}
	if view.Default != bundleapply.PeerControlPlaneKubeconfig {
		t.Fatalf("view must name the conventional default, got %q", view.Default)
	}
}

// The transactional accounting survives the surface boundary: a failed
// apply's error text reports what was rolled back.
func TestBundleApply_FailureCarriesRollbackAccounting(t *testing.T) {
	fix := newBundleFixture(t)
	failing := filepath.Join(fix.home, "failing")
	if err := os.MkdirAll(failing, 0o755); err != nil {
		t.Fatal(err)
	}
	dep := "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: web\n  namespace: demo\nspec:\n  replicas: 1\n"
	if err := os.WriteFile(filepath.Join(failing, "dep.yaml"), []byte(dep), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := fix.srv.BundleApply(context.Background(), BundleApplyParams{
		Bundle: failing, Kubeconfig: fix.peer, TimeoutSeconds: 1, PollSeconds: 1,
	})
	if err == nil {
		t.Fatal("never-ready deployment must fail")
	}
	if !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("error must carry the rollback accounting, got %v", err)
	}
	if len(res.RolledBack) != 1 {
		t.Fatalf("result must report the rollback, got %+v", res)
	}
}
