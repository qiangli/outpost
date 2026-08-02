package appcatalog

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const validAppYAML = `
schemaVersion: 1
id: %s
displayName: Demo
license: Apache-2.0
platforms: [linux/amd64, linux/arm64]
chart:
  repo: https://charts.example.com
  name: demo
  version: 1.2.3
`

func catalogFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, id := range []string{"demo", "cert-manager"} {
		dir := filepath.Join(root, "apps", id)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		manifest := strings.ReplaceAll(validAppYAML, "%s", id)
		if err := os.WriteFile(filepath.Join(dir, "app.yaml"), []byte(manifest), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// demo also ships a values.yaml override; cert-manager does not —
	// values.yaml must stay optional.
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
	if entry.ValuesFile == "" {
		t.Fatalf("expected demo to resolve a values.yaml, got none")
	}

	entry2, err := Resolve(root, "cert-manager")
	if err != nil {
		t.Fatal(err)
	}
	if entry2.ValuesFile != "" {
		t.Fatalf("cert-manager ships no values.yaml, resolved %q", entry2.ValuesFile)
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
