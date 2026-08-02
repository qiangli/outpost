package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/qiangli/outpost/internal/agent/admincore"
)

// Parity, CLI leg: what the operator types maps onto the SAME
// admincore.Appstore* methods the MCP tools (tools_appstore.go) reach —
// same request shape, same fail-closed validation, same venue guard. The
// MCP leg lives in internal/agent/mcpapi/appstore_test.go; the admincore
// behaviour itself in internal/agent/admincore/appstore_test.go. This
// file reuses newBundleCLICore (bundle_test.go) — the same fake-HOME +
// fake-cluster seam the builtins CLI tests use.

func parseAppstoreInstallFlags(t *testing.T, argv []string) *appstoreInstallFlags {
	t.Helper()
	var f appstoreInstallFlags
	cmd := &cobra.Command{Use: "install"}
	addAppstoreTargetFlags(cmd, &f.appstoreTargetFlags)
	cmd.Flags().StringVar(&f.kubeconfig, "kubeconfig", "", "")
	cmd.Flags().StringVar(&f.catalog, "catalog", "", "")
	cmd.Flags().IntVar(&f.timeout, "timeout", 0, "")
	cmd.Flags().IntVar(&f.poll, "poll", 0, "")
	cmd.Flags().BoolVar(&f.allowScaleToZero, "allow-scale-to-zero", false, "")
	cmd.Flags().BoolVar(&f.noRollback, "no-rollback", false, "")
	cmd.Flags().BoolVar(&f.saveKubeconfig, "save-kubeconfig", false, "")
	cmd.Flags().BoolVar(&f.saveCatalog, "save-catalog", false, "")
	cmd.Flags().BoolVar(&f.offline, "offline", false, "")
	if err := cmd.Flags().Parse(argv); err != nil {
		t.Fatalf("parse %v: %v", argv, err)
	}
	return &f
}

func TestAppstoreInstallFlags_MapToAdmincoreParams(t *testing.T) {
	f := parseAppstoreInstallFlags(t, []string{
		"--kubeconfig", "/tmp/peer/k3s.yaml", "--catalog", "/tmp/appstore",
		"--namespace", "user-alice", "--release", "demo-2",
		"--timeout", "120", "--poll", "3",
		"--allow-scale-to-zero", "--no-rollback", "--save-kubeconfig", "--save-catalog",
	})
	got := resolveAppstoreInstallParams("demo", f)
	want := admincore.AppstoreInstallParams{
		ID: "demo", Catalog: "/tmp/appstore", Kubeconfig: "/tmp/peer/k3s.yaml",
		Namespace: "user-alice", Release: "demo-2",
		TimeoutSeconds: 120, PollSeconds: 3,
		AllowScaleToZero: true, NoRollback: true, SaveKubeconfig: true, SaveCatalog: true,
	}
	if got != want {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestAppstoreInstallArgs_KeysMatchMCPTool(t *testing.T) {
	p := admincore.AppstoreInstallParams{
		ID: "demo", Catalog: "/tmp/appstore", Kubeconfig: "/tmp/peer.yaml",
		Namespace: "user-alice", Release: "demo-2",
		TimeoutSeconds: 120, PollSeconds: 3,
		AllowScaleToZero: true, NoRollback: true, SaveKubeconfig: true, SaveCatalog: true,
	}
	args := appstoreInstallArgs(p)
	want := map[string]any{
		"id": "demo", "namespace": "user-alice", "catalog": "/tmp/appstore", "kubeconfig": "/tmp/peer.yaml",
		"release": "demo-2", "timeout_seconds": 120, "poll_seconds": 3,
		"allow_scale_to_zero": true, "no_rollback": true, "save_kubeconfig": true, "save_catalog": true,
	}
	if len(args) != len(want) {
		t.Fatalf("arg keys drifted: got %v want %v", args, want)
	}
	for k, v := range want {
		if args[k] != v {
			t.Fatalf("arg %q = %v, want %v", k, args[k], v)
		}
	}
}

func appstoreCatalogFixture(t *testing.T, root string) string {
	t.Helper()
	catalog := filepath.Join(root, "appstore")
	dir := filepath.Join(catalog, "apps", "demo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "apiVersion: appstore.dhnt.io/v1\nkind: AppEntry\nmetadata:\n  id: demo\n  name: Demo\n  version: \"1.0\"\nspec:\n  chart:\n    repo: https://charts.example.com\n    name: demo\n    version: 1.2.3\n  targetNamespace: \"{{.UserNamespace}}\"\n  rbac:\n    clusterScoped: false\n"
	if err := os.WriteFile(filepath.Join(dir, "app.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	return catalog
}

// The CLI's daemon-free path round-trips install -> status -> uninstall
// -> status through the identical admincore methods the MCP tools call.
func TestAppstoreOffline_RoundTrip(t *testing.T) {
	core, peer, _ := newBundleCLICore(t)
	catalog := appstoreCatalogFixture(t, os.Getenv("HOME"))

	var installBuf bytes.Buffer
	if err := appstoreInstallOffline(context.Background(), core, admincore.AppstoreInstallParams{
		ID: "demo", Catalog: catalog, Kubeconfig: peer, Namespace: "user-alice", TimeoutSeconds: 5,
	}, &installBuf); err != nil {
		t.Fatalf("install: %v", err)
	}
	if !strings.Contains(installBuf.String(), "installed:  demo -> user-alice/demo (1 object(s), 1 confirmed ready)") {
		t.Fatalf("unexpected install output:\n%s", installBuf.String())
	}

	var statusBuf bytes.Buffer
	if err := appstoreStatusOffline(context.Background(), core, admincore.AppstoreStatusParams{
		ID: "demo", Catalog: catalog, Kubeconfig: peer, Namespace: "user-alice",
	}, &statusBuf); err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(statusBuf.String(), "installed=true all_ready=true") {
		t.Fatalf("unexpected status output:\n%s", statusBuf.String())
	}

	var uninstallBuf bytes.Buffer
	if err := appstoreUninstallOffline(context.Background(), core, admincore.AppstoreUninstallParams{
		ID: "demo", Catalog: catalog, Kubeconfig: peer, Namespace: "user-alice",
	}, &uninstallBuf); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if !strings.Contains(uninstallBuf.String(), "uninstalled: demo from user-alice/demo (1 object(s) removed)") {
		t.Fatalf("unexpected uninstall output:\n%s", uninstallBuf.String())
	}

	var finalBuf bytes.Buffer
	if err := appstoreStatusOffline(context.Background(), core, admincore.AppstoreStatusParams{
		ID: "demo", Catalog: catalog, Kubeconfig: peer, Namespace: "user-alice",
	}, &finalBuf); err != nil {
		t.Fatalf("final status: %v", err)
	}
	if !strings.Contains(finalBuf.String(), "installed=false") {
		t.Fatalf("expected not-installed after uninstall:\n%s", finalBuf.String())
	}
}

func TestAppstoreOffline_VenueGuard(t *testing.T) {
	core, _, _ := newBundleCLICore(t)
	catalog := appstoreCatalogFixture(t, os.Getenv("HOME"))
	cloudbox := filepath.Join(os.Getenv("HOME"), ".kube", "outpost.yaml")

	err := appstoreInstallOffline(context.Background(), core, admincore.AppstoreInstallParams{
		ID: "demo", Catalog: catalog, Kubeconfig: cloudbox, Namespace: "user-alice",
	}, &bytes.Buffer{})
	var apiErr *admincore.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 400 || !strings.Contains(apiErr.Msg, "cloudbox") {
		t.Fatalf("cloudbox venue must be refused with a 400, got %v", err)
	}
}

func TestAppstoreOffline_RequiresNamespace(t *testing.T) {
	core, peer, _ := newBundleCLICore(t)
	catalog := appstoreCatalogFixture(t, os.Getenv("HOME"))
	err := appstoreInstallOffline(context.Background(), core, admincore.AppstoreInstallParams{
		ID: "demo", Catalog: catalog, Kubeconfig: peer,
	}, &bytes.Buffer{})
	var apiErr *admincore.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 400 {
		t.Fatalf("missing namespace must fail closed with 400, got %v", err)
	}
}

func TestAppstoreShowOffline_NeverTouchesCluster(t *testing.T) {
	core, _, _ := newBundleCLICore(t)
	catalog := appstoreCatalogFixture(t, os.Getenv("HOME"))
	view, err := core.AppstoreShow(catalog, "demo")
	if err != nil {
		t.Fatalf("AppstoreShow: %v", err)
	}
	if !view.OK || view.ChartName != "demo" {
		t.Fatalf("unexpected show result: %+v", view)
	}
}
