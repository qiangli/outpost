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

const appstoreValidAppYAML = `
schemaVersion: 1
id: demo
displayName: Demo Chart
license: Apache-2.0
platforms: [linux/amd64, linux/arm64]
chart:
  repo: https://charts.example.com
  name: demo
  version: 1.2.3
`

// appstoreCatalogFixture writes an "apps/demo" entry (with a values.yaml
// override) alongside the existing "builtin/headlamp" fixture shape, so
// the two catalogs can coexist under one appstore root like the real
// dhnt/appstore checkout does.
func appstoreCatalogFixture(t *testing.T, root string) string {
	t.Helper()
	catalog := filepath.Join(root, "appstore")
	dir := filepath.Join(catalog, "apps", "demo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.yaml"), []byte(appstoreValidAppYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "values.yaml"), []byte("replicaCount: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return catalog
}

func TestAppstoreInstall_RendersHelmChartAndReusesBundleTransaction(t *testing.T) {
	fix := newBundleFixture(t)
	catalog := appstoreCatalogFixture(t, fix.home)

	res, err := fix.srv.AppstoreInstall(context.Background(), AppstoreInstallParams{
		ID: "demo", Catalog: catalog, Kubeconfig: fix.peer,
		Namespace: "user-alice", SaveCatalog: true, TimeoutSeconds: 5,
	})
	if err != nil {
		t.Fatalf("AppstoreInstall: %v", err)
	}
	if !res.OK || res.Applied != 1 || res.Ready != 1 || !res.CatalogSaved {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.Release != "demo" || res.Namespace != "user-alice" || res.ObjectName != "user-alice-demo" {
		t.Fatalf("unexpected release naming: %+v", res)
	}

	fc, err := conf.LoadFile(filepath.Join(fix.home, "agent.json"))
	if err != nil {
		t.Fatal(err)
	}
	if fc.Cluster == nil || fc.Cluster.BundleCatalog != res.Catalog {
		t.Fatalf("catalog not persisted: %+v", fc.Cluster)
	}

	view, err := fix.srv.AppstoreCatalog("")
	if err != nil || len(view.Apps) != 1 || view.Apps[0] != "demo" {
		t.Fatalf("view=%+v err=%v", view, err)
	}
}

func TestAppstoreInstall_TwoUsersDoNotCollide(t *testing.T) {
	fix := newBundleFixture(t)
	catalog := appstoreCatalogFixture(t, fix.home)

	if _, err := fix.srv.AppstoreInstall(context.Background(), AppstoreInstallParams{
		ID: "demo", Catalog: catalog, Kubeconfig: fix.peer, Namespace: "user-alice", TimeoutSeconds: 5,
	}); err != nil {
		t.Fatalf("install for alice: %v", err)
	}
	if _, err := fix.srv.AppstoreInstall(context.Background(), AppstoreInstallParams{
		ID: "demo", Catalog: catalog, Kubeconfig: fix.peer, Namespace: "user-bob", TimeoutSeconds: 5,
	}); err != nil {
		t.Fatalf("install for bob: %v", err)
	}

	alice, err := fix.srv.AppstoreStatus(context.Background(), AppstoreStatusParams{ID: "demo", Catalog: catalog, Kubeconfig: fix.peer, Namespace: "user-alice"})
	if err != nil || !alice.Installed {
		t.Fatalf("alice status=%+v err=%v", alice, err)
	}
	bob, err := fix.srv.AppstoreStatus(context.Background(), AppstoreStatusParams{ID: "demo", Catalog: catalog, Kubeconfig: fix.peer, Namespace: "user-bob"})
	if err != nil || !bob.Installed {
		t.Fatalf("bob status=%+v err=%v", bob, err)
	}
	if alice.ObjectName == bob.ObjectName {
		t.Fatalf("alice and bob collided on the same object: %q", alice.ObjectName)
	}

	// Uninstalling alice's release must not affect bob's.
	if _, err := fix.srv.AppstoreUninstall(context.Background(), AppstoreUninstallParams{ID: "demo", Catalog: catalog, Kubeconfig: fix.peer, Namespace: "user-alice"}); err != nil {
		t.Fatalf("uninstall alice: %v", err)
	}
	alice, err = fix.srv.AppstoreStatus(context.Background(), AppstoreStatusParams{ID: "demo", Catalog: catalog, Kubeconfig: fix.peer, Namespace: "user-alice"})
	if err != nil || alice.Installed {
		t.Fatalf("alice must be uninstalled: %+v err=%v", alice, err)
	}
	bob, err = fix.srv.AppstoreStatus(context.Background(), AppstoreStatusParams{ID: "demo", Catalog: catalog, Kubeconfig: fix.peer, Namespace: "user-bob"})
	if err != nil || !bob.Installed {
		t.Fatalf("bob must survive alice's uninstall: %+v err=%v", bob, err)
	}
}

func TestAppstoreInstall_RequiresNamespace(t *testing.T) {
	fix := newBundleFixture(t)
	catalog := appstoreCatalogFixture(t, fix.home)
	_, err := fix.srv.AppstoreInstall(context.Background(), AppstoreInstallParams{ID: "demo", Catalog: catalog, Kubeconfig: fix.peer})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 400 {
		t.Fatalf("missing namespace must fail closed with 400: %v", err)
	}
}

func TestAppstoreInstall_FailsClosedOnUnknownAndEscapingIDs(t *testing.T) {
	fix := newBundleFixture(t)
	catalog := appstoreCatalogFixture(t, fix.home)
	for _, id := range []string{"../demo", "missing"} {
		_, err := fix.srv.AppstoreInstall(context.Background(), AppstoreInstallParams{ID: id, Catalog: catalog, Kubeconfig: fix.peer, Namespace: "user-alice"})
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Status != 400 {
			t.Fatalf("%q should fail closed with 400: %v", id, err)
		}
	}
}

func TestAppstoreInstall_FailsClosedOnUnsupportedSchemaLicensePlatform(t *testing.T) {
	fix := newBundleFixture(t)
	catalog := filepath.Join(fix.home, "appstore")

	cases := map[string]string{
		"unsupported schema": `
schemaVersion: 2
id: bad
license: MIT
platforms: [linux/amd64]
chart: {repo: https://x.example.com, name: bad, version: "1.0"}
`,
		"unsupported license": `
schemaVersion: 1
id: bad
license: Proprietary
platforms: [linux/amd64]
chart: {repo: https://x.example.com, name: bad, version: "1.0"}
`,
		"unsupported platform": `
schemaVersion: 1
id: bad
license: MIT
platforms: [windows/amd64]
chart: {repo: https://x.example.com, name: bad, version: "1.0"}
`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(catalog, "apps", "bad")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "app.yaml"), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := fix.srv.AppstoreInstall(context.Background(), AppstoreInstallParams{
				ID: "bad", Catalog: catalog, Kubeconfig: fix.peer, Namespace: "user-alice",
			})
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.Status != 400 {
				t.Fatalf("%s: expected 400 fail-closed, got %v", name, err)
			}
		})
	}
}

func TestAppstoreInstall_VenueGuardRefusesCloudbox(t *testing.T) {
	fix := newBundleFixture(t)
	catalog := appstoreCatalogFixture(t, fix.home)
	_, err := fix.srv.AppstoreInstall(context.Background(), AppstoreInstallParams{
		ID: "demo", Catalog: catalog, Kubeconfig: fix.cloudbox, Namespace: "user-alice",
	})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || !strings.Contains(apiErr.Msg, "cloudbox") {
		t.Fatalf("cloudbox venue guard lost: %v", err)
	}
}

func TestAppstoreUninstall_RoundTripsWithInstall(t *testing.T) {
	fix := newBundleFixture(t)
	catalog := appstoreCatalogFixture(t, fix.home)
	if _, err := fix.srv.AppstoreInstall(context.Background(), AppstoreInstallParams{
		ID: "demo", Catalog: catalog, Kubeconfig: fix.peer, Namespace: "user-alice", TimeoutSeconds: 5,
	}); err != nil {
		t.Fatalf("install: %v", err)
	}
	res, err := fix.srv.AppstoreUninstall(context.Background(), AppstoreUninstallParams{ID: "demo", Catalog: catalog, Kubeconfig: fix.peer, Namespace: "user-alice"})
	if err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if !res.OK || len(res.Deleted) != 1 {
		t.Fatalf("unexpected uninstall result: %+v", res)
	}
	status, err := fix.srv.AppstoreStatus(context.Background(), AppstoreStatusParams{ID: "demo", Catalog: catalog, Kubeconfig: fix.peer, Namespace: "user-alice"})
	if err != nil {
		t.Fatal(err)
	}
	if status.Installed {
		t.Fatalf("uninstalled app must not report installed: %+v", status)
	}
}

func TestAppstoreShow_ValidatesWithoutTouchingCluster(t *testing.T) {
	fix := newBundleFixture(t)
	catalog := appstoreCatalogFixture(t, fix.home)
	view, err := fix.srv.AppstoreShow(catalog, "demo")
	if err != nil {
		t.Fatalf("AppstoreShow: %v", err)
	}
	if !view.OK || view.ChartName != "demo" || view.ChartVersion != "1.2.3" || !view.HasValues {
		t.Fatalf("unexpected show result: %+v", view)
	}
	if len(fix.got) != 0 {
		t.Fatalf("AppstoreShow must never touch a cluster, but a client factory call was recorded: %v", fix.got)
	}
}
