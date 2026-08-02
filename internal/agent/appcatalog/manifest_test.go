package appcatalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func writeApp(t *testing.T, root, id, body string) {
	t.Helper()
	dir := filepath.Join(root, "apps", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadValidManifest(t *testing.T) {
	root := t.TempDir()
	writeApp(t, root, "demo", `
schemaVersion: 1
id: demo
license: MIT
platforms: [linux/amd64]
chart:
  repo: https://charts.example.com
  name: demo
  version: 1.0.0
`)
	entry, err := Resolve(root, "demo")
	if err != nil {
		t.Fatal(err)
	}
	m, values, err := Load(entry)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m.Chart.Name != "demo" || m.Chart.Version != "1.0.0" {
		t.Fatalf("unexpected chart: %+v", m.Chart)
	}
	if values != "" {
		t.Fatalf("expected no values, got %q", values)
	}
}

func TestLoadFailsClosed(t *testing.T) {
	cases := map[string]string{
		"missing schemaVersion": `
id: demo
license: MIT
platforms: [linux/amd64]
chart: {repo: https://x.example.com, name: demo, version: "1.0"}
`,
		"unsupported schemaVersion": `
schemaVersion: 2
id: demo
license: MIT
platforms: [linux/amd64]
chart: {repo: https://x.example.com, name: demo, version: "1.0"}
`,
		"id mismatch": `
schemaVersion: 1
id: not-demo
license: MIT
platforms: [linux/amd64]
chart: {repo: https://x.example.com, name: demo, version: "1.0"}
`,
		"missing license": `
schemaVersion: 1
id: demo
platforms: [linux/amd64]
chart: {repo: https://x.example.com, name: demo, version: "1.0"}
`,
		"proprietary license": `
schemaVersion: 1
id: demo
license: Proprietary
platforms: [linux/amd64]
chart: {repo: https://x.example.com, name: demo, version: "1.0"}
`,
		"no platforms": `
schemaVersion: 1
id: demo
license: MIT
platforms: []
chart: {repo: https://x.example.com, name: demo, version: "1.0"}
`,
		"windows-only platform": `
schemaVersion: 1
id: demo
license: MIT
platforms: [windows/amd64]
chart: {repo: https://x.example.com, name: demo, version: "1.0"}
`,
		"non-https/oci repo": `
schemaVersion: 1
id: demo
license: MIT
platforms: [linux/amd64]
chart: {repo: "file:///etc/passwd", name: demo, version: "1.0"}
`,
		"missing chart version": `
schemaVersion: 1
id: demo
license: MIT
platforms: [linux/amd64]
chart: {repo: https://x.example.com, name: demo}
`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeApp(t, root, "demo", body)
			entry, err := Resolve(root, "demo")
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := Load(entry); err == nil {
				t.Fatalf("case %q: expected Load to fail closed", name)
			}
		})
	}
}

func TestLoadCarriesValuesVerbatim(t *testing.T) {
	root := t.TempDir()
	writeApp(t, root, "demo", `
schemaVersion: 1
id: demo
license: Apache-2.0
platforms: [linux/amd64, linux/arm64]
chart: {repo: https://x.example.com, name: demo, version: "1.0"}
`)
	// Deliberately shell-metacharacter-laden to prove values are never
	// interpolated into a command line — only ever carried as data.
	values := "replicaCount: 2\nextraArgs:\n  - \"$(rm -rf /); `id`; ${IFS}\"\n"
	if err := os.WriteFile(filepath.Join(root, "apps", "demo", "values.yaml"), []byte(values), 0o600); err != nil {
		t.Fatal(err)
	}
	entry, err := Resolve(root, "demo")
	if err != nil {
		t.Fatal(err)
	}
	m, got, err := Load(entry)
	if err != nil {
		t.Fatal(err)
	}
	if got != values {
		t.Fatalf("values mutated: got %q want %q", got, values)
	}

	obj, err := Render(m, got, Target{Namespace: "user-alice", Release: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	vc, found, err := unstructured.NestedString(obj.Object, "spec", "valuesContent")
	if err != nil || !found {
		t.Fatalf("valuesContent not set: found=%v err=%v", found, err)
	}
	if vc != values {
		t.Fatalf("rendered valuesContent = %q, want %q", vc, values)
	}
	if !strings.Contains(vc, "$(rm -rf /)") {
		t.Fatalf("expected the dangerous payload to survive as inert data")
	}
}
