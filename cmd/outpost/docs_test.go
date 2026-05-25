package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestEmbeddedDocsMatchSource — the canonical operator docs live in
// docs/<topic>.md at the repo root. The same files are mirrored under
// cmd/outpost/embedded_docs/ so go:embed can pick them up (the
// directive can't traverse '..' out of the package). This test fails
// loudly when the two copies drift.
func TestEmbeddedDocsMatchSource(t *testing.T) {
	// cmd/outpost test working dir = repo/cmd/outpost; docs/ is two
	// levels up.
	repoRoot := filepath.Join("..", "..")
	for _, d := range docsManifest {
		t.Run(d.Topic, func(t *testing.T) {
			source := filepath.Join(repoRoot, "docs", d.Topic+".md")
			embedded := filepath.Join("embedded_docs", d.Topic+".md")
			srcBytes, err := os.ReadFile(source)
			if err != nil {
				t.Fatalf("read source %s: %v", source, err)
			}
			embBytes, err := os.ReadFile(embedded)
			if err != nil {
				t.Fatalf("read embedded %s: %v", embedded, err)
			}
			if string(srcBytes) != string(embBytes) {
				t.Errorf("docs/%s.md and cmd/outpost/embedded_docs/%s.md have drifted.\n"+
					"Re-sync with: cp docs/%s.md cmd/outpost/embedded_docs/%s.md",
					d.Topic, d.Topic, d.Topic, d.Topic)
			}
		})
	}
}

// TestDocsManifestComplete — every topic listed in docsManifest must
// have a matching embedded file.
func TestDocsManifestComplete(t *testing.T) {
	if err := validateDocsManifest(); err != nil {
		t.Fatal(err)
	}
}
