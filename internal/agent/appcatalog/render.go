package appcatalog

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// HelmChartAPIVersion / HelmChartKind name the k3s built-in
// helm-controller custom resource this package renders into. k3s ships
// the helm-controller and its CRD by default on every node running the
// embedded control plane, so a peer-hosted DKS plane needs no separate
// Helm CLI install, no pinned/managed Helm binary, and no chart-template
// execution inside this process — the controller performs `helm
// upgrade --install` itself, in-cluster, from the fields set below.
const (
	HelmChartAPIVersion = "helm.cattle.io/v1"
	HelmChartKind       = "HelmChart"
)

// HelmChartNamespace is the fixed namespace k3s's helm-controller watches
// for HelmChart custom resources system-wide (the same namespace k3s's
// own built-in HelmChart CRs, e.g. traefik, live in). The actual release
// lands in Target.Namespace via spec.targetNamespace + spec.createNamespace
// — the HelmChart object itself lives here so it can be created before its
// own target (per-user) namespace exists.
const HelmChartNamespace = "kube-system"

// maxObjectNameLen mirrors the Kubernetes DNS-1123 label limit; the
// derived HelmChart object name (and therefore the Helm release name,
// since the controller uses the object's own metadata.name as the
// release) must fit it.
const maxObjectNameLen = 63

// AppLabelKey / NamespaceAnnotationKey / ReleaseAnnotationKey stamp the
// rendered object with the catalog id and the caller's install
// coordinates, so Status/Uninstall (which only need to recompute a
// name — see ReleaseObject) and any operator inspecting the cluster with
// `kubectl` can trace a HelmChart CR back to its appstore origin.
const (
	AppLabelKey            = "outpost.dhnt.io/app-id"
	NamespaceAnnotationKey = "outpost.dhnt.io/app-namespace"
	ReleaseAnnotationKey   = "outpost.dhnt.io/app-release"
)

// Target names the per-user/per-instance install coordinates. Both
// fields are required and validated — there is no default namespace, so
// two different users (or two installs of the same app for one user)
// can never collide silently.
type Target struct {
	// Namespace is the target/user namespace the chart is installed
	// into (spec.targetNamespace). Required.
	Namespace string
	// Release distinguishes multiple instances of the same app within
	// one namespace. Defaults to the app id when empty (one instance is
	// the common case).
	Release string
}

// objectName derives the deterministic HelmChart object name — and
// therefore the Helm release name, since the k3s helm-controller runs
// `helm upgrade --install <metadata.name> ...` — from Namespace+Release.
// Install, Status, and Uninstall all recompute the identical name from
// the identical (namespace, release) pair, so the three operations can
// never target different objects for what the operator considers "the
// same install".
func (t Target) objectName() (string, error) {
	ns := strings.TrimSpace(t.Namespace)
	rel := strings.TrimSpace(t.Release)
	if ns == "" {
		return "", fmt.Errorf("target namespace is required (per-user isolation has no default)")
	}
	if !validName.MatchString(ns) {
		return "", fmt.Errorf("invalid target namespace %q", ns)
	}
	if rel == "" {
		return "", fmt.Errorf("release name is required")
	}
	if !validName.MatchString(rel) {
		return "", fmt.Errorf("invalid release name %q", rel)
	}
	name := ns + "-" + rel
	if len(name) > maxObjectNameLen {
		return "", fmt.Errorf("namespace+release name %q exceeds %d characters", name, maxObjectNameLen)
	}
	return name, nil
}

// ReleaseObject builds the addressable (kind/namespace/name) reference
// Status/Uninstall Get/Delete against — the identical object Render
// would apply for the same manifest+target, without needing the
// manifest or values at all (a pure, deterministic name derivation).
func ReleaseObject(t Target) (*unstructured.Unstructured, error) {
	name, err := t.objectName()
	if err != nil {
		return nil, err
	}
	obj := &unstructured.Unstructured{Object: map[string]any{}}
	obj.SetAPIVersion(HelmChartAPIVersion)
	obj.SetKind(HelmChartKind)
	obj.SetNamespace(HelmChartNamespace)
	obj.SetName(name)
	return obj, nil
}

// Render builds the helm.cattle.io/v1 HelmChart object that installs
// m's chart into t.Namespace under release t.Release, embedding
// valuesYAML byte-for-byte as spec.valuesContent.
//
// Every field is set through unstructured.SetNestedField on a typed Go
// value — there is no string formatting, YAML templating, or shell
// invocation anywhere in this path, so nothing from the manifest or
// values file is ever interpolated into a command line. The k3s
// helm-controller reconciling the resulting object is what actually
// executes Helm, inside the cluster, from these structured fields.
func Render(m *Manifest, valuesYAML string, t Target) (*unstructured.Unstructured, error) {
	if m == nil {
		return nil, fmt.Errorf("nil app manifest")
	}
	obj, err := ReleaseObject(t)
	if err != nil {
		return nil, err
	}
	obj.SetLabels(map[string]string{AppLabelKey: m.ID})
	obj.SetAnnotations(map[string]string{
		NamespaceAnnotationKey: t.Namespace,
		ReleaseAnnotationKey:   t.Release,
	})

	fields := []struct {
		val  any
		path []string
	}{
		{t.Namespace, []string{"spec", "targetNamespace"}},
		{true, []string{"spec", "createNamespace"}},
		{m.Chart.Name, []string{"spec", "chart"}},
		{m.Chart.Version, []string{"spec", "version"}},
		{m.Chart.Repo, []string{"spec", "repo"}},
	}
	for _, f := range fields {
		if err := unstructured.SetNestedField(obj.Object, f.val, f.path...); err != nil {
			return nil, fmt.Errorf("render app %q: set %v: %w", m.ID, f.path, err)
		}
	}
	if strings.TrimSpace(valuesYAML) != "" {
		if err := unstructured.SetNestedField(obj.Object, valuesYAML, "spec", "valuesContent"); err != nil {
			return nil, fmt.Errorf("render app %q: set values: %w", m.ID, err)
		}
	}
	return obj, nil
}
