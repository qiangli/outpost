package builtincatalog

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func catalogFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{"headlamp", "core-storage"} {
		dir := filepath.Join(root, "builtin", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "install.yaml"), []byte("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: "+name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestResolveAndList(t *testing.T) {
	root := catalogFixture(t)
	entry, err := Resolve(root, "headlamp")
	if err != nil {
		t.Fatal(err)
	}
	if entry.Name != "headlamp" || filepath.Base(entry.Manifest) != "install.yaml" {
		t.Fatalf("unexpected entry: %+v", entry)
	}
	_, names, err := List(root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(names, []string{"core-storage", "headlamp"}) {
		t.Fatalf("names = %v", names)
	}
}

func TestResolveFailsClosed(t *testing.T) {
	root := catalogFixture(t)
	for _, name := range []string{"../secret", "Headlamp", "missing"} {
		if _, err := Resolve(root, name); err == nil {
			t.Fatalf("Resolve(%q) unexpectedly succeeded", name)
		}
	}
	outside := filepath.Join(t.TempDir(), "outside.yaml")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "builtin", "escape")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "install.yaml")); err != nil {
		t.Skip(err)
	}
	_, err := Resolve(root, "escape")
	if err == nil || !strings.Contains(err.Error(), "escapes catalog") {
		t.Fatalf("symlink escape: %v", err)
	}
}
