package mcpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/admincore"
	"github.com/qiangli/outpost/internal/agent/bundleapply"
	"github.com/qiangli/outpost/internal/agent/conf"
)

// fakeBundleClient — deterministic in-memory bundleapply.ResourceClient
// (no apiserver) so the MCP protocol roundtrip can be tested end to end.
type fakeBundleClient struct {
	mu    sync.Mutex
	store map[string]*unstructured.Unstructured
}

func (f *fakeBundleClient) key(o *unstructured.Unstructured) string {
	return o.GetKind() + "|" + o.GetNamespace() + "|" + o.GetName()
}

func (f *fakeBundleClient) Apply(_ context.Context, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.store[f.key(obj)] = obj.DeepCopy()
	return obj, nil
}

func (f *fakeBundleClient) Get(_ context.Context, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	live, ok := f.store[f.key(obj)]
	if !ok {
		return nil, fmt.Errorf("fake: %s: %w", f.key(obj), bundleapply.ErrNotFound)
	}
	return live.DeepCopy(), nil
}

func (f *fakeBundleClient) Delete(_ context.Context, obj *unstructured.Unstructured) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.store, f.key(obj))
	return nil
}

func (f *fakeBundleClient) ServesGVK(_ context.Context, _ schema.GroupVersionKind) (bool, error) {
	return true, nil
}

// newBundleTestMCP is newTestMCP plus a fake-HOME kube layout and an
// injected BundleApplyClient — the venue guard still runs for real inside
// admincore (the factory only ever sees an accepted, canonicalized path).
func newBundleTestMCP(t *testing.T, token string) (*httptest.Server, peerPaths) {
	t.Helper()
	tmp := t.TempDir()
	tmp, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("XDG_CACHE_HOME", tmp)

	peerDir := filepath.Join(tmp, ".kube", "outpost-control-plane")
	if err := os.MkdirAll(peerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	paths := peerPaths{
		peer:     filepath.Join(peerDir, "k3s.yaml"),
		cloudbox: filepath.Join(tmp, ".kube", "outpost.yaml"),
		bundle:   filepath.Join(tmp, "bundle"),
		catalog:  filepath.Join(tmp, "appstore"),
	}
	for _, p := range []string{paths.peer, paths.cloudbox} {
		if err := os.WriteFile(p, []byte("apiVersion: v1\nkind: Config\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(paths.bundle, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: demo\n---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cfg\n  namespace: demo\n"
	if err := os.WriteFile(filepath.Join(paths.bundle, "all.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	builtinDir := filepath.Join(paths.catalog, "builtin", "headlamp")
	if err := os.MkdirAll(builtinDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(builtinDir, "install.yaml"), []byte("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: headlamp\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(tmp, "agent.json")
	if err := conf.SaveFile(configPath, &conf.FileConfig{}); err != nil {
		t.Fatal(err)
	}
	core, err := admincore.New(admincore.Deps{
		ConfigPath: configPath,
		Apps:       agent.NewAppRegistry(),
		BundleApplyClient: func(string) (bundleapply.ResourceClient, error) {
			return &fakeBundleClient{store: map[string]*unstructured.Unstructured{}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	mcpSrv, err := New(Deps{Core: core, Token: token, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	httpSrv := httptest.NewServer(mcpSrv.Handler())
	t.Cleanup(httpSrv.Close)
	return httpSrv, paths
}

type peerPaths struct {
	peer, cloudbox, bundle, catalog string
}

func connectBundleMCP(t *testing.T) (*mcp.ClientSession, peerPaths) {
	t.Helper()
	const token = "bundle-token-1234"
	httpSrv, paths := newBundleTestMCP(t, token)
	transport := &mcp.StreamableClientTransport{
		Endpoint: httpSrv.URL,
		HTTPClient: &http.Client{Transport: &bearerRT{
			token: token,
			base:  http.DefaultTransport,
		}},
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0.0.0"}, nil)
	session, err := client.Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session, paths
}

// Parity: the MCP tool is registered and lands on admincore.BundleApply —
// the identical method the CLI and the script's runner reach.
func TestBundleTools_ApplyOverProtocol(t *testing.T) {
	session, paths := connectBundleMCP(t)

	// Both bundle tools are registered.
	tools, err := session.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, tl := range tools.Tools {
		found[tl.Name] = true
	}
	for _, want := range []string{"outpost_apply_bundle", "outpost_bundle_kubeconfig", "outpost_install_builtin", "outpost_bundle_catalog"} {
		if !found[want] {
			t.Fatalf("tool %s not registered", want)
		}
	}

	body, isErr := callJSON(t, session, "outpost_apply_bundle", map[string]any{
		"kubeconfig":      paths.peer,
		"bundle":          paths.bundle,
		"timeout_seconds": 5,
		"save_kubeconfig": true,
	})
	if isErr {
		t.Fatalf("apply reported an error: %s", body)
	}
	var out struct {
		OK              bool     `json:"ok"`
		Kubeconfig      string   `json:"kubeconfig"`
		Applied         int      `json:"applied"`
		Ready           int      `json:"ready"`
		Created         []string `json:"created"`
		KubeconfigSaved bool     `json:"kubeconfig_saved"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	if !out.OK || out.Applied != 2 || out.Ready != 2 || len(out.Created) != 2 || !out.KubeconfigSaved {
		t.Fatalf("unexpected apply result: %s", body)
	}
	if out.Kubeconfig != paths.peer {
		t.Fatalf("result must report the canonical venue, got %q", out.Kubeconfig)
	}

	// The save persisted through the SAME admincore method the direct
	// tests exercise, and the view tool reads it back.
	body, isErr = callJSON(t, session, "outpost_bundle_kubeconfig", map[string]any{})
	if isErr {
		t.Fatalf("view reported an error: %s", body)
	}
	var view struct {
		Kubeconfig string `json:"kubeconfig"`
		Default    string `json:"default"`
	}
	if err := json.Unmarshal([]byte(body), &view); err != nil {
		t.Fatal(err)
	}
	if view.Kubeconfig != paths.peer {
		t.Fatalf("persisted venue must be visible over MCP, got %q", view.Kubeconfig)
	}
	if view.Default != bundleapply.PeerControlPlaneKubeconfig {
		t.Fatalf("view must name the conventional default, got %q", view.Default)
	}

	body, isErr = callJSON(t, session, "outpost_install_builtin", map[string]any{
		"name": "headlamp", "catalog": paths.catalog, "kubeconfig": paths.peer,
		"timeout_seconds": 5, "save_catalog": true,
	})
	if isErr || !strings.Contains(body, `"name":"headlamp"`) || !strings.Contains(body, `"catalog_saved":true`) {
		t.Fatalf("built-in install: isErr=%v body=%s", isErr, body)
	}
	body, isErr = callJSON(t, session, "outpost_bundle_catalog", map[string]any{})
	if isErr || !strings.Contains(body, `"headlamp"`) {
		t.Fatalf("catalog view: isErr=%v body=%s", isErr, body)
	}
}

// Parity: the venue guard fires identically over MCP — the cloudbox
// kubeconfig (and a symlinked route to it) is refused as a tool error
// with admincore's message, before any client is built.
func TestBundleTools_VenueGuardOverProtocol(t *testing.T) {
	session, paths := connectBundleMCP(t)

	body, isErr := callJSON(t, session, "outpost_apply_bundle", map[string]any{
		"kubeconfig": paths.cloudbox,
		"bundle":     paths.bundle,
	})
	if !isErr || !strings.Contains(body, "cloudbox") {
		t.Fatalf("cloudbox venue must be refused over MCP, got isErr=%v body=%s", isErr, body)
	}

	link := filepath.Join(filepath.Dir(paths.bundle), "sneaky.yaml")
	if err := os.Symlink(paths.cloudbox, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	body, isErr = callJSON(t, session, "outpost_apply_bundle", map[string]any{
		"kubeconfig": link,
		"bundle":     paths.bundle,
	})
	if !isErr || !strings.Contains(body, "cloudbox") {
		t.Fatalf("symlinked cloudbox venue must be refused over MCP, got isErr=%v body=%s", isErr, body)
	}
}
