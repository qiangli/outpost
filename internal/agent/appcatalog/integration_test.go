package appcatalog

import (
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// testdataCatalog is the checked-in real-shape appstore fixture root
// (testdata/appstore) — redis + langfuse entries copied structurally from
// the genuine dhnt/appstore catalog, so a schema regression is caught by a
// hermetic test without needing a sibling checkout present.
func testdataCatalog(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("testdata", "appstore"))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// TestRealShapeFixturesLoad proves the redis + langfuse fixtures — the real
// appstore.dhnt.io/v1 AppEntry shape, not the retired
// schemaVersion/license/platforms invention — resolve, validate, and load
// cleanly, and render into a HelmChart CR with the expected chart coordinates.
func TestRealShapeFixturesLoad(t *testing.T) {
	catalog := testdataCatalog(t)

	root, ids, err := List(catalog)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(ids) != 2 || ids[0] != "langfuse" || ids[1] != "redis" {
		t.Fatalf("unexpected catalog ids: %v", ids)
	}

	cases := []struct {
		id        string
		chart     string
		version   string
		repo      string
		hasValues bool
	}{
		{"redis", "redis", "20.1.0", "https://charts.bitnami.com/bitnami", true},
		{"langfuse", "langfuse", "1.2.13", "https://langfuse.github.io/langfuse-k8s", false},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			entry, err := Resolve(root, tc.id)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			m, values, err := Load(entry)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if m.APIVersion != APIVersion || m.Kind != Kind {
				t.Fatalf("wrong envelope: %s/%s", m.APIVersion, m.Kind)
			}
			if m.Metadata.ID != tc.id {
				t.Fatalf("metadata.id = %q, want %q", m.Metadata.ID, tc.id)
			}
			if m.Spec.Chart.Name != tc.chart || m.Spec.Chart.Version != tc.version || m.Spec.Chart.Repo != tc.repo {
				t.Fatalf("unexpected chart: %+v", m.Spec.Chart)
			}
			if (values != "") != tc.hasValues {
				t.Fatalf("hasValues=%v but values=%q", tc.hasValues, values)
			}

			obj, err := Render(m, values, Target{Namespace: "user-alice", Release: tc.id})
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			gotChart, _, _ := unstructured.NestedString(obj.Object, "spec", "chart")
			if gotChart != tc.chart {
				t.Fatalf("rendered spec.chart = %q, want %q", gotChart, tc.chart)
			}
		})
	}
}

// TestSiblingAppstoreCheckout resolves and loads the ACTUAL dhnt/appstore
// sibling checkout when one is present, so a genuine catalog entry the
// fixtures don't happen to cover still has to satisfy this package's
// validation. It skips (never fails) when no checkout is found — the
// checked-in fixtures above are the always-on guard.
//
// Discovery order: $OUTPOST_APPSTORE_CATALOG, then the umbrella/standalone
// sibling locations relative to the repo root (../appstore, ./appstore).
func TestSiblingAppstoreCheckout(t *testing.T) {
	catalog := siblingAppstore(t)
	if catalog == "" {
		t.Skip("no sibling dhnt/appstore checkout found; run with a checkout at ../appstore or $OUTPOST_APPSTORE_CATALOG to exercise it")
	}

	root, ids, err := List(catalog)
	if err != nil {
		t.Fatalf("List(%q): %v", catalog, err)
	}
	if len(ids) == 0 {
		t.Fatalf("sibling appstore %q lists no apps", root)
	}
	t.Logf("validating %d apps from sibling appstore %q", len(ids), root)
	for _, id := range ids {
		entry, err := Resolve(root, id)
		if err != nil {
			t.Errorf("Resolve(%q): %v", id, err)
			continue
		}
		if _, _, err := Load(entry); err != nil {
			t.Errorf("Load(%q): %v", id, err)
		}
	}
}

// siblingAppstore locates a real appstore checkout without relying on cwd
// (which is the package dir under `go test`): an explicit env override, else
// the sibling of / within the repo root discovered by walking up to go.mod.
// Returns "" when none has an apps/ directory.
func siblingAppstore(t *testing.T) string {
	t.Helper()
	if env := os.Getenv("OUTPOST_APPSTORE_CATALOG"); env != "" {
		if hasAppsDir(env) {
			return env
		}
		return ""
	}
	repo := repoRoot(t)
	if repo == "" {
		return ""
	}
	for _, c := range []string{
		filepath.Join(filepath.Dir(repo), "appstore"),
		filepath.Join(repo, "appstore"),
	} {
		if hasAppsDir(c) {
			return c
		}
	}
	return ""
}

func hasAppsDir(catalog string) bool {
	info, err := os.Stat(filepath.Join(catalog, "apps"))
	return err == nil && info.IsDir()
}

// repoRoot walks up from the package dir to the directory holding go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}
