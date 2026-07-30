package recipebuilder

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPackInlineRecipeRoundTripIsDeterministicAndWhitelisted(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "Dockerfile", "FROM scratch\n")
	writeTestFile(t, root, "cmd/app/main.go", "package main\n")
	writeTestFile(t, root, "private/secret", "must-not-ship")

	spec := InlineRecipeSpec{
		Name: "example", Tag: "v1", LocalRef: "localhost/cluster/example",
		ContextDir: root, Dockerfile: "Dockerfile",
		Includes:   []string{"cmd/app", "Dockerfile"},
		BaseImages: []string{"scratch"},
	}
	var first, second bytes.Buffer
	if err := PackInlineRecipe(&first, spec); err != nil {
		t.Fatalf("first PackInlineRecipe() error = %v", err)
	}
	if err := PackInlineRecipe(&second, spec); err != nil {
		t.Fatalf("second PackInlineRecipe() error = %v", err)
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatal("identical inputs produced different recipe bytes")
	}

	r := parseRecipe(first.String())
	if err := r.validate(); err != nil {
		t.Fatalf("generated recipe is invalid: %v", err)
	}
	raw, err := decodeInlineArchive(r.ContextArchive, r.ContextSha256)
	if err != nil {
		t.Fatalf("decode generated archive: %v", err)
	}
	dest := t.TempDir()
	if err := extractInlineArchive(raw, dest); err != nil {
		t.Fatalf("extract generated archive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "Dockerfile")); err != nil {
		t.Fatalf("Dockerfile missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "cmd", "app", "main.go")); err != nil {
		t.Fatalf("included source missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "private", "secret")); !os.IsNotExist(err) {
		t.Fatalf("non-whitelisted source was packed: err=%v", err)
	}
}

func TestPackInlineRecipeRejectsTraversalSymlinkAndMissingDockerfile(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "Dockerfile", "FROM scratch\n")
	writeTestFile(t, root, "outside", "outside")
	if err := os.Symlink(filepath.Join(root, "outside"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	base := InlineRecipeSpec{
		Name: "example", LocalRef: "localhost/cluster/example",
		ContextDir: root, Dockerfile: "Dockerfile",
	}
	for _, tc := range []struct {
		name     string
		includes []string
	}{
		{name: "traversal", includes: []string{"../outside"}},
		{name: "symlink", includes: []string{"Dockerfile", "link"}},
		{name: "dockerfile omitted", includes: []string{"outside"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := base
			spec.Includes = tc.includes
			if err := PackInlineRecipe(&bytes.Buffer{}, spec); err == nil {
				t.Fatal("unsafe/incomplete context was accepted")
			}
		})
	}
}

func TestPackInlineRecipeRejectsCompressedOversize(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "Dockerfile", "FROM scratch\n")
	var payload bytes.Buffer
	for i := 0; payload.Len() <= maxInlineArchiveBytes+8192; i++ {
		block := sha256.Sum256([]byte(fmt.Sprintf("block-%d", i)))
		payload.Write(block[:])
	}
	if err := os.WriteFile(filepath.Join(root, "payload"), payload.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := InlineRecipeSpec{
		Name: "example", LocalRef: "localhost/cluster/example",
		ContextDir: root, Dockerfile: "Dockerfile",
		Includes: []string{"Dockerfile", "payload"},
	}
	err := PackInlineRecipe(&bytes.Buffer{}, spec)
	if err == nil || !strings.Contains(err.Error(), "compressed context") {
		t.Fatalf("PackInlineRecipe() error = %v, want compressed-size rejection", err)
	}
}

func writeTestFile(t *testing.T, root, name, body string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
