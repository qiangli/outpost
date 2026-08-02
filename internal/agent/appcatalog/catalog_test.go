package appcatalog

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const validAppYAML = `
apiVersion: appstore.dhnt.io/v1
kind: AppEntry
metadata:
  id: %s
  name: Demo
  version: "1.0"
spec:
  chart:
    repo: https://charts.example.com
    name: demo
    version: 1.2.3
  targetNamespace: "{{.UserNamespace}}"
  rbac:
    clusterScoped: false
  defaultValuesFile: %s
`

// catalogFixture writes two apps: "demo" DECLARES defaultValuesFile:
// values.yaml and ships it; "cert-manager" declares an empty
// defaultValuesFile and ships no values.yaml at all — proving values stay
// optional when the manifest says so, exercised through Load (the only
// place spec.defaultValuesFile is now resolved).
func catalogFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	declared := map[string]string{"demo": "values.yaml", "cert-manager": ""}
	for _, id := range []string{"demo", "cert-manager"} {
		dir := filepath.Join(root, "apps", id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		manifest := fmt.Sprintf(validAppYAML, id, declared[id])
		if err := os.WriteFile(filepath.Join(dir, "app.yaml"), []byte(manifest), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "apps", "demo", "values.yaml"), []byte("replicas: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestResolveAndList(t *testing.T) {
	root := catalogFixture(t)
	entry, err := Resolve(root, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if entry.ID != "demo" || filepath.Base(entry.AppFile) != "app.yaml" {
		t.Fatalf("unexpected entry: %+v", entry)
	}
	if _, values, err := Load(entry); err != nil || values == "" {
		t.Fatalf("expected demo's declared values.yaml to load: values=%q err=%v", values, err)
	}

	entry2, err := Resolve(root, "cert-manager")
	if err != nil {
		t.Fatal(err)
	}
	if _, values, err := Load(entry2); err != nil || values != "" {
		t.Fatalf("cert-manager declares no defaultValuesFile, expected no values: values=%q err=%v", values, err)
	}

	_, ids, err := List(root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ids, []string{"cert-manager", "demo"}) {
		t.Fatalf("ids = %v", ids)
	}
}

func TestResolveFailsClosed(t *testing.T) {
	root := catalogFixture(t)
	for _, id := range []string{"../secret", "Demo", "missing"} {
		if _, err := Resolve(root, id); err == nil {
			t.Fatalf("Resolve(%q) unexpectedly succeeded", id)
		}
	}

	outside := filepath.Join(t.TempDir(), "outside.yaml")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "apps", "escape")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "app.yaml")); err != nil {
		t.Skip(err)
	}
	_, err := Resolve(root, "escape")
	if err == nil || !strings.Contains(err.Error(), "escapes catalog") {
		t.Fatalf("symlink escape: %v", err)
	}
}

func TestResolveCatalogRequiresAppsDir(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "builtin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveCatalog(root); err == nil {
		t.Fatalf("catalog without an apps/ dir must be refused")
	}
}
