package appcatalog

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func validManifest() *Manifest {
	return &Manifest{
		APIVersion: APIVersion,
		Kind:       Kind,
		Metadata:   Metadata{ID: "demo", Name: "Demo"},
		Spec: Spec{
			Chart: ChartRef{Repo: "https://charts.example.com", Name: "demo", Version: "1.2.3"},
		},
	}
}

func TestRenderProducesHelmChartCR(t *testing.T) {
	m := validManifest()
	obj, err := Render(m, "replicas: 2\n", Target{Namespace: "user-alice", Release: "demo"})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if obj.GetAPIVersion() != HelmChartAPIVersion || obj.GetKind() != HelmChartKind {
		t.Fatalf("unexpected GVK: %s/%s", obj.GetAPIVersion(), obj.GetKind())
	}
	if obj.GetNamespace() != HelmChartNamespace {
		t.Fatalf("HelmChart CR must live in %q, got %q", HelmChartNamespace, obj.GetNamespace())
	}
	if obj.GetName() != "user-alice-demo" {
		t.Fatalf("unexpected object name: %q", obj.GetName())
	}
	ns, _, _ := unstructured.NestedString(obj.Object, "spec", "targetNamespace")
	if ns != "user-alice" {
		t.Fatalf("targetNamespace = %q", ns)
	}
	createNS, _, _ := unstructured.NestedBool(obj.Object, "spec", "createNamespace")
	if !createNS {
		t.Fatalf("createNamespace must be true")
	}
	chart, _, _ := unstructured.NestedString(obj.Object, "spec", "chart")
	version, _, _ := unstructured.NestedString(obj.Object, "spec", "version")
	repo, _, _ := unstructured.NestedString(obj.Object, "spec", "repo")
	if chart != "demo" || version != "1.2.3" || repo != "https://charts.example.com" {
		t.Fatalf("unexpected chart spec: chart=%q version=%q repo=%q", chart, version, repo)
	}
	if obj.GetLabels()[AppLabelKey] != "demo" {
		t.Fatalf("missing app-id label: %v", obj.GetLabels())
	}
}

// Two different users installing the same app id must render to two
// distinct HelmChart objects (distinct release names) — the namespace
// isolation the peer-DKS appstore path is required to preserve.
func TestRenderIsolatesPerUserNamespace(t *testing.T) {
	m := validManifest()
	a, err := Render(m, "", Target{Namespace: "user-alice", Release: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Render(m, "", Target{Namespace: "user-bob", Release: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if a.GetName() == b.GetName() {
		t.Fatalf("two users' installs collided on object name %q", a.GetName())
	}

	// Two instances for the SAME user must also be distinguishable via
	// an explicit distinct release.
	c, err := Render(m, "", Target{Namespace: "user-alice", Release: "demo-staging"})
	if err != nil {
		t.Fatal(err)
	}
	if a.GetName() == c.GetName() {
		t.Fatalf("same-user distinct releases collided on object name %q", a.GetName())
	}
}

func TestRenderFailsClosedOnTarget(t *testing.T) {
	m := validManifest()
	cases := []Target{
		{Namespace: "", Release: "demo"},
		{Namespace: "user-alice", Release: ""},
		{Namespace: "../etc", Release: "demo"},
		{Namespace: "User-Alice", Release: "demo"},
		{Namespace: strings.Repeat("a", 60), Release: "demo"},
	}
	for _, target := range cases {
		if _, err := Render(m, "", target); err == nil {
			t.Fatalf("Render(%+v) unexpectedly succeeded", target)
		}
	}
}

func TestReleaseObjectMatchesRender(t *testing.T) {
	m := validManifest()
	target := Target{Namespace: "user-alice", Release: "demo"}
	rendered, err := Render(m, "", target)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := ReleaseObject(target)
	if err != nil {
		t.Fatal(err)
	}
	if ref.GetKind() != rendered.GetKind() || ref.GetNamespace() != rendered.GetNamespace() || ref.GetName() != rendered.GetName() {
		t.Fatalf("ReleaseObject diverged from Render: %+v vs %+v", ref, rendered)
	}
}
