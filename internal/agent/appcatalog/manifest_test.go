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
apiVersion: appstore.dhnt.io/v1
kind: AppEntry
metadata:
  id: demo
  name: Demo
  version: "1.0"
spec:
  chart:
    repo: https://charts.example.com
    name: demo
    version: 1.0.0
  targetNamespace: "{{.UserNamespace}}"
  rbac:
    clusterScoped: false
  defaultValuesFile: ""
`)
	entry, err := Resolve(root, "demo")
	if err != nil {
		t.Fatal(err)
	}
	m, values, err := Load(entry)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if m.Spec.Chart.Name != "demo" || m.Spec.Chart.Version != "1.0.0" {
		t.Fatalf("unexpected chart: %+v", m.Spec.Chart)
	}
	if m.Metadata.Name != "Demo" || m.Metadata.Version != "1.0" {
		t.Fatalf("unexpected metadata: %+v", m.Metadata)
	}
	if values != "" {
		t.Fatalf("expected no values, got %q", values)
	}
}

func TestLoadFailsClosed(t *testing.T) {
	cases := map[string]string{
		"missing apiVersion": `
kind: AppEntry
metadata: {id: demo}
spec:
  chart: {repo: https://x.example.com, name: demo, version: "1.0"}
`,
		"wrong apiVersion": `
apiVersion: appstore.dhnt.io/v2
kind: AppEntry
metadata: {id: demo}
spec:
  chart: {repo: https://x.example.com, name: demo, version: "1.0"}
`,
		"wrong kind": `
apiVersion: appstore.dhnt.io/v1
kind: Application
metadata: {id: demo}
spec:
  chart: {repo: https://x.example.com, name: demo, version: "1.0"}
`,
		"id mismatch": `
apiVersion: appstore.dhnt.io/v1
kind: AppEntry
metadata: {id: not-demo}
spec:
  chart: {repo: https://x.example.com, name: demo, version: "1.0"}
`,
		"missing id": `
apiVersion: appstore.dhnt.io/v1
kind: AppEntry
metadata: {name: Demo}
spec:
  chart: {repo: https://x.example.com, name: demo, version: "1.0"}
`,
		"non-https/oci repo": `
apiVersion: appstore.dhnt.io/v1
kind: AppEntry
metadata: {id: demo}
spec:
  chart: {repo: "file:///etc/passwd", name: demo, version: "1.0"}
`,
		"missing chart version": `
apiVersion: appstore.dhnt.io/v1
kind: AppEntry
metadata: {id: demo}
spec:
  chart: {repo: https://x.example.com, name: demo}
`,
		"floating latest version": `
apiVersion: appstore.dhnt.io/v1
kind: AppEntry
metadata: {id: demo}
spec:
  chart: {repo: https://x.example.com, name: demo, version: latest}
`,
		"unsafe version": `
apiVersion: appstore.dhnt.io/v1
kind: AppEntry
metadata: {id: demo}
spec:
  chart: {repo: https://x.example.com, name: demo, version: "1.0; rm -rf /"}
`,
		"missing chart name": `
apiVersion: appstore.dhnt.io/v1
kind: AppEntry
metadata: {id: demo}
spec:
  chart: {repo: https://x.example.com, version: "1.0"}
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
apiVersion: appstore.dhnt.io/v1
kind: AppEntry
metadata: {id: demo}
spec:
  chart: {repo: https://x.example.com, name: demo, version: "1.0"}
  defaultValuesFile: values.yaml
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

// TestLoadDefaultValuesFileAuthoritative pins down spec.defaultValuesFile
// as the SOLE source of truth for the values override: empty means no
// values (even when an unrelated values.yaml sits right next to app.yaml),
// a declared name is honored over the "values.yaml" convention, a
// declared-but-missing file fails closed, and traversal / symlink escape
// out of the app's own directory are refused.
func TestLoadDefaultValuesFileAuthoritative(t *testing.T) {
	t.Run("empty declaration means no values, even if values.yaml exists", func(t *testing.T) {
		root := t.TempDir()
		writeApp(t, root, "demo", `
apiVersion: appstore.dhnt.io/v1
kind: AppEntry
metadata: {id: demo}
spec:
  chart: {repo: https://x.example.com, name: demo, version: "1.0"}
  defaultValuesFile: ""
`)
		if err := os.WriteFile(filepath.Join(root, "apps", "demo", "values.yaml"), []byte("replicas: 2\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		entry, err := Resolve(root, "demo")
		if err != nil {
			t.Fatal(err)
		}
		_, values, err := Load(entry)
		if err != nil {
			t.Fatal(err)
		}
		if values != "" {
			t.Fatalf("expected no values when defaultValuesFile is empty, got %q", values)
		}
	})

	t.Run("declared but missing file fails closed", func(t *testing.T) {
		root := t.TempDir()
		writeApp(t, root, "demo", `
apiVersion: appstore.dhnt.io/v1
kind: AppEntry
metadata: {id: demo}
spec:
  chart: {repo: https://x.example.com, name: demo, version: "1.0"}
  defaultValuesFile: overrides.yaml
`)
		entry, err := Resolve(root, "demo")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := Load(entry); err == nil {
			t.Fatal("expected Load to fail closed on a declared-but-missing defaultValuesFile")
		}
	})

	t.Run("declared name is honored over the values.yaml convention", func(t *testing.T) {
		root := t.TempDir()
		writeApp(t, root, "demo", `
apiVersion: appstore.dhnt.io/v1
kind: AppEntry
metadata: {id: demo}
spec:
  chart: {repo: https://x.example.com, name: demo, version: "1.0"}
  defaultValuesFile: overrides.yaml
`)
		if err := os.WriteFile(filepath.Join(root, "apps", "demo", "values.yaml"), []byte("wrong: true\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		want := "right: true\n"
		if err := os.WriteFile(filepath.Join(root, "apps", "demo", "overrides.yaml"), []byte(want), 0o600); err != nil {
			t.Fatal(err)
		}
		entry, err := Resolve(root, "demo")
		if err != nil {
			t.Fatal(err)
		}
		_, values, err := Load(entry)
		if err != nil {
			t.Fatal(err)
		}
		if values != want {
			t.Fatalf("values = %q, want %q (the declared file, not the values.yaml convention)", values, want)
		}
	})

	t.Run("traversal outside the app directory is rejected", func(t *testing.T) {
		root := t.TempDir()
		writeApp(t, root, "other", `
apiVersion: appstore.dhnt.io/v1
kind: AppEntry
metadata: {id: other}
spec:
  chart: {repo: https://x.example.com, name: other, version: "1.0"}
`)
		if err := os.WriteFile(filepath.Join(root, "apps", "other", "values.yaml"), []byte("secret: true\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		writeApp(t, root, "demo", `
apiVersion: appstore.dhnt.io/v1
kind: AppEntry
metadata: {id: demo}
spec:
  chart: {repo: https://x.example.com, name: demo, version: "1.0"}
  defaultValuesFile: "../other/values.yaml"
`)
		entry, err := Resolve(root, "demo")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := Load(entry); err == nil {
			t.Fatal("expected Load to reject a traversal defaultValuesFile")
		}
	})

	t.Run("symlink escape out of the catalog is rejected", func(t *testing.T) {
		root := t.TempDir()
		outside := filepath.Join(t.TempDir(), "outside-values.yaml")
		if err := os.WriteFile(outside, []byte("x: 1\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		writeApp(t, root, "demo", `
apiVersion: appstore.dhnt.io/v1
kind: AppEntry
metadata: {id: demo}
spec:
  chart: {repo: https://x.example.com, name: demo, version: "1.0"}
  defaultValuesFile: values.yaml
`)
		if err := os.Symlink(outside, filepath.Join(root, "apps", "demo", "values.yaml")); err != nil {
			t.Skip(err)
		}
		entry, err := Resolve(root, "demo")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := Load(entry); err == nil || !strings.Contains(err.Error(), "escapes catalog") {
			t.Fatalf("expected symlink escape rejection, got %v", err)
		}
	})
}
