package admincore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
