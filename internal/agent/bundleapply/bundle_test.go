package bundleapply

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

const multiDoc = `
apiVersion: apps/v1
kind: Deployment
metadata:
  name: web
  namespace: demo
spec:
  replicas: 2
---
apiVersion: v1
kind: Namespace
metadata:
  name: demo
---
# a comment-only document is skipped, not an error
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: cfg
  namespace: demo
`

func TestLoadBundleOrdersNamespaceFirst(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "all.yaml", multiDoc)

	b, err := LoadBundle(dir)
	if err != nil {
		t.Fatalf("LoadBundle: %v", err)
	}
	if len(b.Objects) != 3 {
		t.Fatalf("want 3 objects (comment doc skipped), got %d", len(b.Objects))
	}
	// Namespace must be first even though it was declared second in the file.
	if k := b.Objects[0].GetKind(); k != "Namespace" {
		t.Fatalf("want Namespace applied first, got %s", k)
	}
	// ConfigMap (rank 12) before Deployment (rank 44).
	if k := b.Objects[1].GetKind(); k != "ConfigMap" {
		t.Fatalf("want ConfigMap second, got %s", k)
	}
	if k := b.Objects[2].GetKind(); k != "Deployment" {
		t.Fatalf("want Deployment last, got %s", k)
	}
}

func TestLoadBundleSingleFile(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "ns.yaml", "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: demo\n")
	b, err := LoadBundle(p)
	if err != nil {
		t.Fatalf("LoadBundle: %v", err)
	}
	if len(b.Objects) != 1 || b.Objects[0].GetName() != "demo" {
		t.Fatalf("unexpected objects: %+v", b.Objects)
	}
}

func TestLoadBundleEmptyDirIsError(t *testing.T) {
	dir := t.TempDir()
	_, err := LoadBundle(dir)
	if !errors.Is(err, ErrEmptyBundle) {
		t.Fatalf("empty dir must fail with ErrEmptyBundle (no silent success), got %v", err)
	}
}

func TestLoadBundleOnlyCommentsIsError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "c.yaml", "# nothing here\n---\n# still nothing\n")
	_, err := LoadBundle(dir)
	if !errors.Is(err, ErrEmptyBundle) {
		t.Fatalf("comment-only bundle must be ErrEmptyBundle, got %v", err)
	}
}

func TestLoadBundleMissingKindIsError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "bad.yaml", "apiVersion: v1\nmetadata:\n  name: x\n")
	_, err := LoadBundle(filepath.Join(dir, "bad.yaml"))
	if err == nil {
		t.Fatal("a document missing kind must fail loudly, not be dropped")
	}
	if errors.Is(err, ErrEmptyBundle) {
		t.Fatalf("missing-kind should be a decode error, not empty-bundle: %v", err)
	}
}

func TestLoadBundleMissingNameIsError(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "bad.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata: {}\n")
	_, err := LoadBundle(filepath.Join(dir, "bad.yaml"))
	if err == nil {
		t.Fatal("a document missing metadata.name must fail loudly")
	}
}

func TestLoadBundleMissingPathIsError(t *testing.T) {
	_, err := LoadBundle(filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Fatal("a missing bundle path must fail")
	}
}

func TestLoadBundleEmptyPathIsError(t *testing.T) {
	if _, err := LoadBundle("   "); err == nil {
		t.Fatal("empty path must fail")
	}
}
