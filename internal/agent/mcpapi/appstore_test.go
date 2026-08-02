package mcpapi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Parity: every appstore-apps tool is registered and lands on the
// matching admincore.Appstore* method — the identical methods the CLI
// and offline path reach.
func TestAppstoreTools_InstallStatusUninstallOverProtocol(t *testing.T) {
	session, paths := connectBundleMCP(t)

	tools, err := session.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, tl := range tools.Tools {
		found[tl.Name] = true
	}
	for _, want := range []string{
		"outpost_appstore_apps", "outpost_appstore_app",
		"outpost_install_appstore_app", "outpost_appstore_app_status", "outpost_uninstall_appstore_app",
	} {
		if !found[want] {
			t.Fatalf("tool %s not registered", want)
		}
	}

	// Catalog listing.
	body, isErr := callJSON(t, session, "outpost_appstore_apps", map[string]any{"catalog": paths.catalog})
	if isErr {
		t.Fatalf("catalog reported an error: %s", body)
	}
	var catalogOut struct {
		Apps []string `json:"apps"`
	}
	if err := json.Unmarshal([]byte(body), &catalogOut); err != nil {
		t.Fatal(err)
	}
	if len(catalogOut.Apps) != 1 || catalogOut.Apps[0] != "demo" {
		t.Fatalf("unexpected catalog: %s", body)
	}

	// Show never requires or touches a kubeconfig.
	body, isErr = callJSON(t, session, "outpost_appstore_app", map[string]any{"id": "demo", "catalog": paths.catalog})
	if isErr {
		t.Fatalf("show reported an error: %s", body)
	}
	var showOut struct {
		ChartName    string `json:"chart_name"`
		ChartVersion string `json:"chart_version"`
	}
	if err := json.Unmarshal([]byte(body), &showOut); err != nil {
		t.Fatal(err)
	}
	if showOut.ChartName != "demo" || showOut.ChartVersion != "1.2.3" {
		t.Fatalf("unexpected show result: %s", body)
	}

	// Install into a per-user namespace.
	body, isErr = callJSON(t, session, "outpost_install_appstore_app", map[string]any{
		"id": "demo", "catalog": paths.catalog, "kubeconfig": paths.peer,
		"namespace": "user-alice", "timeout_seconds": 5,
	})
	if isErr {
		t.Fatalf("install reported an error: %s", body)
	}
	var installOut struct {
		OK         bool   `json:"ok"`
		Applied    int    `json:"applied"`
		Ready      int    `json:"ready"`
		ObjectName string `json:"object_name"`
		Namespace  string `json:"namespace"`
		Release    string `json:"release"`
	}
	if err := json.Unmarshal([]byte(body), &installOut); err != nil {
		t.Fatal(err)
	}
	if !installOut.OK || installOut.Applied != 1 || installOut.Ready != 1 {
		t.Fatalf("unexpected install result: %s", body)
	}
	if installOut.Namespace != "user-alice" || installOut.Release != "demo" || installOut.ObjectName != "user-alice-demo" {
		t.Fatalf("unexpected release naming: %s", body)
	}

	// Status must reflect the install through the identical resolution.
	body, isErr = callJSON(t, session, "outpost_appstore_app_status", map[string]any{
		"id": "demo", "catalog": paths.catalog, "kubeconfig": paths.peer, "namespace": "user-alice",
	})
	if isErr {
		t.Fatalf("status reported an error: %s", body)
	}
	var statusOut struct {
		Installed bool `json:"installed"`
		AllReady  bool `json:"all_ready"`
	}
	if err := json.Unmarshal([]byte(body), &statusOut); err != nil {
		t.Fatal(err)
	}
	if !statusOut.Installed || !statusOut.AllReady {
		t.Fatalf("expected installed+ready after install: %s", body)
	}

	// Uninstall, then confirm status flips.
	body, isErr = callJSON(t, session, "outpost_uninstall_appstore_app", map[string]any{
		"id": "demo", "catalog": paths.catalog, "kubeconfig": paths.peer, "namespace": "user-alice",
	})
	if isErr {
		t.Fatalf("uninstall reported an error: %s", body)
	}
	var uninstallOut struct {
		OK      bool     `json:"ok"`
		Deleted []string `json:"deleted"`
	}
	if err := json.Unmarshal([]byte(body), &uninstallOut); err != nil {
		t.Fatal(err)
	}
	if !uninstallOut.OK || len(uninstallOut.Deleted) != 1 {
		t.Fatalf("unexpected uninstall result: %s", body)
	}

	body, isErr = callJSON(t, session, "outpost_appstore_app_status", map[string]any{
		"id": "demo", "catalog": paths.catalog, "kubeconfig": paths.peer, "namespace": "user-alice",
	})
	if isErr {
		t.Fatalf("post-uninstall status reported an error: %s", body)
	}
	if err := json.Unmarshal([]byte(body), &statusOut); err != nil {
		t.Fatal(err)
	}
	if statusOut.Installed {
		t.Fatalf("expected not-installed after uninstall: %s", body)
	}
}

func TestAppstoreTools_VenueGuardRefusesCloudbox(t *testing.T) {
	session, paths := connectBundleMCP(t)
	body, isErr := callJSON(t, session, "outpost_install_appstore_app", map[string]any{
		"id": "demo", "catalog": paths.catalog, "kubeconfig": paths.cloudbox, "namespace": "user-alice",
	})
	if !isErr || !strings.Contains(body, "cloudbox") {
		t.Fatalf("cloudbox venue guard lost: isErr=%v body=%s", isErr, body)
	}
}

func TestAppstoreTools_InstallRequiresNamespace(t *testing.T) {
	session, paths := connectBundleMCP(t)
	body, isErr := callJSON(t, session, "outpost_install_appstore_app", map[string]any{
		"id": "demo", "catalog": paths.catalog, "kubeconfig": paths.peer,
	})
	if !isErr {
		t.Fatalf("missing namespace must fail closed, got: %s", body)
	}
}
