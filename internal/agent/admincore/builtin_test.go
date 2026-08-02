package admincore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/qiangli/outpost/internal/agent/conf"
)

func builtinCatalogFixture(t *testing.T, root string) string {
	t.Helper()
	catalog := filepath.Join(root, "appstore")
	dir := filepath.Join(catalog, "builtin", "headlamp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: headlamp\n"
	if err := os.WriteFile(filepath.Join(dir, "install.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	return catalog
}

func TestBuiltinInstall_ResolvesCatalogAndReusesBundleTransaction(t *testing.T) {
	fix := newBundleFixture(t)
	catalog := builtinCatalogFixture(t, fix.home)
	res, err := fix.srv.BuiltinInstall(context.Background(), BuiltinInstallParams{
		Name: "headlamp", Catalog: catalog, Kubeconfig: fix.peer,
		SaveCatalog: true, TimeoutSeconds: 5,
	})
	if err != nil {
		t.Fatalf("BuiltinInstall: %v", err)
	}
	if !res.OK || res.Name != "headlamp" || res.Applied != 1 || res.Ready != 1 || !res.CatalogSaved {
		t.Fatalf("unexpected result: %+v", res)
	}
	if !strings.HasSuffix(res.Manifest, filepath.Join("builtin", "headlamp", "install.yaml")) {
		t.Fatalf("unexpected manifest: %q", res.Manifest)
	}
	fc, err := conf.LoadFile(filepath.Join(fix.home, "agent.json"))
	if err != nil {
		t.Fatal(err)
	}
	if fc.Cluster == nil || fc.Cluster.BundleCatalog != res.Catalog {
		t.Fatalf("catalog not persisted: %+v", fc.Cluster)
	}
	view, err := fix.srv.BundleCatalog("")
	if err != nil || len(view.Builtins) != 1 || view.Builtins[0] != "headlamp" {
		t.Fatalf("view=%+v err=%v", view, err)
	}
}

func TestBuiltinInstall_FailsClosedAndPreservesVenueGuard(t *testing.T) {
	fix := newBundleFixture(t)
	catalog := builtinCatalogFixture(t, fix.home)
	for _, name := range []string{"../headlamp", "missing"} {
		_, err := fix.srv.BuiltinInstall(context.Background(), BuiltinInstallParams{Name: name, Catalog: catalog, Kubeconfig: fix.peer})
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Status != 400 {
			t.Fatalf("%q should fail closed with 400: %v", name, err)
		}
	}
	_, err := fix.srv.BuiltinInstall(context.Background(), BuiltinInstallParams{Name: "headlamp", Catalog: catalog, Kubeconfig: fix.cloudbox})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || !strings.Contains(apiErr.Msg, "cloudbox") {
		t.Fatalf("cloudbox venue guard lost: %v", err)
	}
}

// BuiltinStatus resolves the same manifest BuiltinInstall applies (same
// catalog resolution, same fail-closed name handling) and reuses
// BundleStatus underneath.
func TestBuiltinStatus_ResolvesCatalogAndReflectsInstallState(t *testing.T) {
	fix := newBundleFixture(t)
	catalog := builtinCatalogFixture(t, fix.home)

	before, err := fix.srv.BuiltinStatus(context.Background(), BuiltinStatusParams{Name: "headlamp", Catalog: catalog, Kubeconfig: fix.peer})
	if err != nil {
		t.Fatalf("BuiltinStatus before install: %v", err)
	}
	if before.Installed || before.Name != "headlamp" {
		t.Fatalf("unexpected pre-install status: %+v", before)
	}

	if _, err := fix.srv.BuiltinInstall(context.Background(), BuiltinInstallParams{Name: "headlamp", Catalog: catalog, Kubeconfig: fix.peer, TimeoutSeconds: 5}); err != nil {
		t.Fatalf("BuiltinInstall: %v", err)
	}
	after, err := fix.srv.BuiltinStatus(context.Background(), BuiltinStatusParams{Name: "headlamp", Catalog: catalog, Kubeconfig: fix.peer})
	if err != nil {
		t.Fatalf("BuiltinStatus after install: %v", err)
	}
	if !after.Installed || !after.AllReady {
		t.Fatalf("unexpected post-install status: %+v", after)
	}
}

func TestBuiltinStatus_FailsClosedAndPreservesVenueGuard(t *testing.T) {
	fix := newBundleFixture(t)
	catalog := builtinCatalogFixture(t, fix.home)
	for _, name := range []string{"../headlamp", "missing"} {
		_, err := fix.srv.BuiltinStatus(context.Background(), BuiltinStatusParams{Name: name, Catalog: catalog, Kubeconfig: fix.peer})
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Status != 400 {
			t.Fatalf("%q should fail closed with 400: %v", name, err)
		}
	}
	_, err := fix.srv.BuiltinStatus(context.Background(), BuiltinStatusParams{Name: "headlamp", Catalog: catalog, Kubeconfig: fix.cloudbox})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || !strings.Contains(apiErr.Msg, "cloudbox") {
		t.Fatalf("cloudbox venue guard lost: %v", err)
	}
}

// BuiltinUninstall reuses BuiltinInstall's exact catalog resolution and
// BundleUninstall's deletion mechanics — install then uninstall leaves
// the cluster clean.
func TestBuiltinUninstall_RoundTripsWithInstall(t *testing.T) {
	fix := newBundleFixture(t)
	catalog := builtinCatalogFixture(t, fix.home)
	if _, err := fix.srv.BuiltinInstall(context.Background(), BuiltinInstallParams{Name: "headlamp", Catalog: catalog, Kubeconfig: fix.peer, TimeoutSeconds: 5}); err != nil {
		t.Fatalf("BuiltinInstall: %v", err)
	}

	res, err := fix.srv.BuiltinUninstall(context.Background(), BuiltinUninstallParams{Name: "headlamp", Catalog: catalog, Kubeconfig: fix.peer})
	if err != nil {
		t.Fatalf("BuiltinUninstall: %v", err)
	}
	if !res.OK || res.Name != "headlamp" || len(res.Deleted) != 1 {
		t.Fatalf("unexpected uninstall result: %+v", res)
	}

	status, err := fix.srv.BuiltinStatus(context.Background(), BuiltinStatusParams{Name: "headlamp", Catalog: catalog, Kubeconfig: fix.peer})
	if err != nil {
		t.Fatalf("BuiltinStatus after uninstall: %v", err)
	}
	if status.Installed {
		t.Fatalf("uninstalled built-in must not report installed: %+v", status)
	}
}

func TestBuiltinUninstall_FailsClosedAndPreservesVenueGuard(t *testing.T) {
	fix := newBundleFixture(t)
	catalog := builtinCatalogFixture(t, fix.home)
	for _, name := range []string{"../headlamp", "missing"} {
		_, err := fix.srv.BuiltinUninstall(context.Background(), BuiltinUninstallParams{Name: name, Catalog: catalog, Kubeconfig: fix.peer})
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Status != 400 {
			t.Fatalf("%q should fail closed with 400: %v", name, err)
		}
	}
	_, err := fix.srv.BuiltinUninstall(context.Background(), BuiltinUninstallParams{Name: "headlamp", Catalog: catalog, Kubeconfig: fix.cloudbox})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || !strings.Contains(apiErr.Msg, "cloudbox") {
		t.Fatalf("cloudbox venue guard lost: %v", err)
	}
}

// installerCatalogFixture writes a headlamp-shaped built-in whose product
// is bootstrapped by an installer Job with ttlSecondsAfterFinished — the
// Job declares its durable outputs per the bundleapply lifecycle contract.
func installerCatalogFixture(t *testing.T, root string) string {
	t.Helper()
	catalog := filepath.Join(root, "appstore")
	dir := filepath.Join(catalog, "builtin", "headlamp")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `apiVersion: v1
kind: Namespace
metadata:
  name: headlamp
---
apiVersion: batch/v1
kind: Job
metadata:
  name: install-headlamp
  namespace: headlamp
  annotations:
    outpost.dhnt.io/lifecycle: installer
    outpost.dhnt.io/installs: "apps/v1:Deployment:headlamp:headlamp"
spec:
  ttlSecondsAfterFinished: 30
`
	if err := os.WriteFile(filepath.Join(dir, "install.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	return catalog
}

// seedReadyDeployment puts a rolled-out Deployment into the fake cluster —
// the live product a completed installer leaves behind.
func seedReadyDeployment(t *testing.T, fc *fakeBundleClient, ns, name string) {
	t.Helper()
	dep := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name": name, "namespace": ns, "generation": int64(1),
		},
		"spec": map[string]any{"replicas": int64(1)},
		"status": map[string]any{
			"observedGeneration": int64(1),
			"updatedReplicas":    int64(1),
			"readyReplicas":      int64(1),
			"availableReplicas":  int64(1),
		},
	}}
	if _, err := fc.Apply(context.Background(), dep); err != nil {
		t.Fatal(err)
	}
}

// The live regression this exists for: the product is Running, but the
// installer Job was reaped by ttlSecondsAfterFinished — status must
// report installed=true by asserting the declared durable outputs, not
// count the intentionally-gone Job as an absent object.
func TestBuiltinStatus_InstallerJobTTLDeletedStillInstalled(t *testing.T) {
	fix := newBundleFixture(t)
	catalog := installerCatalogFixture(t, fix.home)
	ns := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "headlamp"},
	}}
	if _, err := fix.fakeClient.Apply(context.Background(), ns); err != nil {
		t.Fatal(err)
	}
	seedReadyDeployment(t, fix.fakeClient, "headlamp", "headlamp")
	// The installer Job is deliberately NOT in the cluster.

	res, err := fix.srv.BuiltinStatus(context.Background(), BuiltinStatusParams{Name: "headlamp", Catalog: catalog, Kubeconfig: fix.peer})
	if err != nil {
		t.Fatalf("BuiltinStatus: %v", err)
	}
	if !res.Installed || !res.AllReady {
		t.Fatalf("running product with a TTL-reaped installer must report installed+ready: %+v", res)
	}
	var sawInstaller, sawOutput bool
	for _, o := range res.Objects {
		sawInstaller = sawInstaller || (o.Installer && o.Kind == "Job")
		sawOutput = sawOutput || (o.DeclaredOutput && o.Kind == "Deployment" && o.Ready)
	}
	if !sawInstaller || !sawOutput {
		t.Fatalf("status must surface the installer and its declared output: %+v", res.Objects)
	}

	// Fail closed: the same bundle with only the Namespace skeleton (a
	// failed install's residue) is NOT installed.
	if err := fix.fakeClient.Delete(context.Background(), &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "headlamp", "namespace": "headlamp"},
	}}); err != nil {
		t.Fatal(err)
	}
	res, err = fix.srv.BuiltinStatus(context.Background(), BuiltinStatusParams{Name: "headlamp", Catalog: catalog, Kubeconfig: fix.peer})
	if err != nil {
		t.Fatalf("BuiltinStatus: %v", err)
	}
	if res.Installed || res.AllReady {
		t.Fatalf("surviving Namespace after a failed install must not report installed: %+v", res)
	}
}

// Uninstall removes the actual installed product (the declared outputs),
// not only the bootstrap manifest — and converges even though the
// installer Job itself is already gone.
func TestBuiltinUninstall_RemovesInstallerProduct(t *testing.T) {
	fix := newBundleFixture(t)
	catalog := installerCatalogFixture(t, fix.home)
	ns := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Namespace", "metadata": map[string]any{"name": "headlamp"},
	}}
	if _, err := fix.fakeClient.Apply(context.Background(), ns); err != nil {
		t.Fatal(err)
	}
	seedReadyDeployment(t, fix.fakeClient, "headlamp", "headlamp")

	res, err := fix.srv.BuiltinUninstall(context.Background(), BuiltinUninstallParams{Name: "headlamp", Catalog: catalog, Kubeconfig: fix.peer})
	if err != nil {
		t.Fatalf("BuiltinUninstall: %v", err)
	}
	if !res.OK || len(res.Failed) != 0 {
		t.Fatalf("uninstall must converge: %+v", res)
	}
	if len(fix.fakeClient.store) != 0 {
		t.Fatalf("product left behind after uninstall: %v", fix.fakeClient.store)
	}
}
