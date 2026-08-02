package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/qiangli/outpost/internal/agent/admincore"
	"github.com/qiangli/outpost/internal/agent/bundleapply"
	"github.com/qiangli/outpost/internal/agent/conf"
)

// Parity, CLI leg: what the operator types maps onto the SAME
// admincore.BundleApply the MCP tool and the script's runner reach —
// same request shape, same venue guard, same validation. The MCP leg of
// the proof lives in internal/agent/mcpapi/bundle_test.go; the admincore
// behaviour itself in internal/agent/admincore/bundle_test.go.

// parseBundleApplyFlags runs a real argv through the real flag set.
func parseBundleApplyFlags(t *testing.T, argv []string) *bundleApplyFlags {
	t.Helper()
	var f bundleApplyFlags
	cmd := &cobra.Command{Use: "apply"}
	addBundleApplyFlags(cmd, &f)
	if err := cmd.Flags().Parse(argv); err != nil {
		t.Fatalf("parse %v: %v", argv, err)
	}
	return &f
}

func TestBundleApplyFlags_MapToAdmincoreParams(t *testing.T) {
	f := parseBundleApplyFlags(t, []string{
		"--kubeconfig", "/tmp/peer/k3s.yaml",
		"--timeout", "120", "--poll", "3", "--crd-timeout", "30",
		"--allow-scale-to-zero", "--no-rollback", "--save-kubeconfig",
	})
	got := resolveBundleApplyParams("./bundle", f)
	want := admincore.BundleApplyParams{
		Kubeconfig:        "/tmp/peer/k3s.yaml",
		Bundle:            "./bundle",
		TimeoutSeconds:    120,
		PollSeconds:       3,
		CRDTimeoutSeconds: 30,
		AllowScaleToZero:  true,
		NoRollback:        true,
		SaveKubeconfig:    true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("params mismatch:\n got %+v\nwant %+v", got, want)
	}
}

// The MCP argument map must speak the outpost_apply_bundle tool's arg
// names (the agent.json naming convention) — a drifted key would be
// silently dropped by the tool's schema and the daemon would apply with
// a default instead of the operator's value.
func TestBundleApplyArgs_KeysMatchMCPTool(t *testing.T) {
	p := admincore.BundleApplyParams{
		Kubeconfig:        "/tmp/peer/k3s.yaml",
		Bundle:            "./bundle",
		TimeoutSeconds:    120,
		PollSeconds:       3,
		CRDTimeoutSeconds: 30,
		AllowScaleToZero:  true,
		NoRollback:        true,
		SaveKubeconfig:    true,
	}
	args := bundleApplyArgs(p)
	var keys []string
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{
		"allow_scale_to_zero", "bundle", "crd_timeout_seconds", "kubeconfig",
		"no_rollback", "poll_seconds", "save_kubeconfig", "timeout_seconds",
	}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("arg keys drifted from the MCP tool's schema:\n got %v\nwant %v", keys, want)
	}

	// Zero values are omitted so the daemon's documented defaults apply.
	minimal := bundleApplyArgs(admincore.BundleApplyParams{Bundle: "./bundle"})
	if len(minimal) != 1 || minimal["bundle"] != "./bundle" {
		t.Fatalf("minimal args should carry only the bundle, got %v", minimal)
	}
}

// cliFakeBundleClient is a deterministic in-memory ResourceClient — no
// apiserver, no cloudbox.
type cliFakeBundleClient struct {
	mu    sync.Mutex
	store map[string]*unstructured.Unstructured
}

func (f *cliFakeBundleClient) key(o *unstructured.Unstructured) string {
	return o.GetKind() + "|" + o.GetNamespace() + "|" + o.GetName()
}

func (f *cliFakeBundleClient) Apply(_ context.Context, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.store[f.key(obj)] = obj.DeepCopy()
	return obj, nil
}

func (f *cliFakeBundleClient) Get(_ context.Context, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	live, ok := f.store[f.key(obj)]
	if !ok {
		return nil, fmt.Errorf("fake: %s: %w", f.key(obj), bundleapply.ErrNotFound)
	}
	return live.DeepCopy(), nil
}

func (f *cliFakeBundleClient) Delete(_ context.Context, obj *unstructured.Unstructured) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.store, f.key(obj))
	return nil
}

func (f *cliFakeBundleClient) ServesGVK(_ context.Context, _ schema.GroupVersionKind) (bool, error) {
	return true, nil
}

// newBundleCLICore builds a real admincore.Server in a fake HOME with an
// injected fake cluster — the exact seam the daemon uses, so the venue
// guard still runs for real inside admincore.
func newBundleCLICore(t *testing.T) (*admincore.Server, string, string) {
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
	peer := filepath.Join(peerDir, "k3s.yaml")
	cloudbox := filepath.Join(tmp, ".kube", "outpost.yaml")
	for _, p := range []string{peer, cloudbox} {
		if err := os.WriteFile(p, []byte("apiVersion: v1\nkind: Config\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	bundleDir := filepath.Join(tmp, "bundle")
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: demo\n---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cfg\n  namespace: demo\n"
	if err := os.WriteFile(filepath.Join(bundleDir, "all.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(tmp, "agent.json")
	if err := conf.SaveFile(configPath, &conf.FileConfig{}); err != nil {
		t.Fatal(err)
	}
	core, err := admincore.New(admincore.Deps{
		ConfigPath: configPath,
		BundleApplyClient: func(string) (bundleapply.ResourceClient, error) {
			return &cliFakeBundleClient{store: map[string]*unstructured.Unstructured{}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return core, peer, bundleDir
}

// The CLI's daemon-free path lands on admincore.BundleApply — the same
// method the MCP tool calls — and renders its accounting.
func TestBundleApplyOffline_ReachesAdmincore(t *testing.T) {
	core, peer, bundleDir := newBundleCLICore(t)
	var buf bytes.Buffer
	p := resolveBundleApplyParams(bundleDir, parseBundleApplyFlags(t,
		[]string{"--kubeconfig", peer, "--timeout", "5", "--poll", "1"}))
	if err := bundleApplyOffline(context.Background(), core, p, &buf); err != nil {
		t.Fatalf("offline apply: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "applied:    2 object(s), 2 confirmed ready") {
		t.Fatalf("expected the apply accounting, got:\n%s", out)
	}
	if !strings.Contains(out, peer) {
		t.Fatalf("expected the canonical venue in the output, got:\n%s", out)
	}
}

// The venue guard runs identically on the CLI path: the cloudbox
// kubeconfig is refused, symlinked spellings included, before any client
// exists.
func TestBundleApplyOffline_VenueGuard(t *testing.T) {
	core, _, bundleDir := newBundleCLICore(t)
	home := os.Getenv("HOME")

	sneaky := filepath.Join(home, "sneaky.yaml")
	if err := os.Symlink(filepath.Join(home, ".kube", "outpost.yaml"), sneaky); err != nil {
		t.Fatal(err)
	}
	for _, kubeconfig := range []string{
		filepath.Join(home, ".kube", "outpost.yaml"),
		sneaky,
	} {
		p := resolveBundleApplyParams(bundleDir, parseBundleApplyFlags(t,
			[]string{"--kubeconfig", kubeconfig}))
		err := bundleApplyOffline(context.Background(), core, p, &bytes.Buffer{})
		if err == nil {
			t.Fatalf("cloudbox venue %q must be refused", kubeconfig)
		}
		var apiErr *admincore.APIError
		if !errors.As(err, &apiErr) || apiErr.Status != 400 {
			t.Fatalf("venue refusal should be the admincore 400, got %v", err)
		}
	}
}
