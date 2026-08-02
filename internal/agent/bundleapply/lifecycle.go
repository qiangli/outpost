package bundleapply

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Installer-style bundle lifecycle.
//
// Some bundles bootstrap their product through an installer Job (often
// with ttlSecondsAfterFinished, so the completed Job is garbage-collected
// by design — the appstore headlamp built-in's install-headlamp Job is the
// canonical case). For such bundles, "is the Job present" is the wrong
// installed-ness question in both directions: a completed-then-reaped Job
// is not evidence of absence, and a surviving Namespace/RBAC skeleton
// after a failed install is not evidence of presence. The manifest
// contract below makes the durable product explicit so status and
// uninstall can reason about it — and fail closed when they cannot.
const (
	// AnnotationLifecycle marks how an object participates in the bundle
	// lifecycle. The only recognized value is LifecycleInstaller; any
	// other value is a hard error (an unparseable signal must fail, not
	// vanish).
	AnnotationLifecycle = "outpost.dhnt.io/lifecycle"
	// LifecycleInstaller marks a bootstrap object (typically a Job with
	// ttlSecondsAfterFinished) that is EXPECTED to complete and may then
	// be garbage-collected. Its absence alone proves nothing — the
	// declared outputs are the durable evidence.
	LifecycleInstaller = "installer"
	// AnnotationInstalls declares the durable post-install resources an
	// installer produces. REQUIRED on every LifecycleInstaller object —
	// without it, an absent installer is indistinguishable from one that
	// never ran, so the contract refuses the manifest outright. Entries
	// are separated by commas and/or whitespace, each of the form
	//
	//	<apiVersion>:<Kind>:<namespace>:<name>
	//
	// with an empty namespace for cluster-scoped resources, e.g.
	//
	//	apps/v1:Deployment:headlamp:headlamp,
	//	rbac.authorization.k8s.io/v1:ClusterRoleBinding::headlamp-admin
	AnnotationInstalls = "outpost.dhnt.io/installs"
)

// isInstaller reports whether obj is annotated as a lifecycle installer.
// It only answers the recognized value; validateLifecycle is what rejects
// unknown lifecycle values as a hard error.
func isInstaller(obj *unstructured.Unstructured) bool {
	return obj.GetAnnotations()[AnnotationLifecycle] == LifecycleInstaller
}

// installerOutputs parses obj's declared durable outputs into identity
// stubs (apiVersion/kind/namespace/name only) suitable for Get/Delete.
// A LifecycleInstaller with a missing, empty, or malformed declaration is
// a hard error — the fail-closed half of the contract: without declared
// outputs there is no durable evidence to assert, so the bundle is
// refused rather than guessed at.
func installerOutputs(obj *unstructured.Unstructured) ([]*unstructured.Unstructured, error) {
	raw := obj.GetAnnotations()[AnnotationInstalls]
	entries := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	if len(entries) == 0 {
		return nil, fmt.Errorf("bundleapply: installer %s %s declares no %s outputs — required so an absent installer (e.g. ttlSecondsAfterFinished) can be told apart from one that never ran",
			obj.GetKind(), nsName(obj), AnnotationInstalls)
	}
	var out []*unstructured.Unstructured
	for _, entry := range entries {
		parts := strings.Split(entry, ":")
		if len(parts) != 4 || parts[0] == "" || parts[1] == "" || parts[3] == "" {
			return nil, fmt.Errorf("bundleapply: installer %s %s: malformed %s entry %q (want <apiVersion>:<Kind>:<namespace>:<name>, empty namespace for cluster-scoped)",
				obj.GetKind(), nsName(obj), AnnotationInstalls, entry)
		}
		stub := &unstructured.Unstructured{Object: map[string]any{}}
		stub.SetAPIVersion(parts[0])
		stub.SetKind(parts[1])
		if parts[2] != "" {
			stub.SetNamespace(parts[2])
		}
		stub.SetName(parts[3])
		out = append(out, stub)
	}
	return out, nil
}

// validateLifecycle enforces the installer contract across a decoded
// bundle: every lifecycle annotation must carry a recognized value, and
// every installer must declare parseable outputs. Run at LoadBundle time
// so apply, status, and uninstall all refuse a contract-violating
// manifest identically, before touching any cluster.
func validateLifecycle(objs []*unstructured.Unstructured) error {
	for _, obj := range objs {
		val, ok := obj.GetAnnotations()[AnnotationLifecycle]
		if !ok {
			continue
		}
		if val != LifecycleInstaller {
			return fmt.Errorf("bundleapply: %s %s: unknown %s value %q (only %q is recognized)",
				obj.GetKind(), nsName(obj), AnnotationLifecycle, val, LifecycleInstaller)
		}
		if _, err := installerOutputs(obj); err != nil {
			return err
		}
	}
	return nil
}

// expandWithOutputs returns the bundle objects plus every installer's
// declared outputs (deduplicated by kind/namespace/name against the
// bundle and each other), re-sorted by apply rank so the synthesized
// stubs slot into the same dependency order as if they had been bundle
// objects. Uninstall uses this so the actual installed product —
// including cluster-scoped outputs a Namespace deletion would never
// touch — is removed, not just the bootstrap manifest.
func expandWithOutputs(objs []*unstructured.Unstructured) ([]*unstructured.Unstructured, error) {
	seen := make(map[string]bool, len(objs))
	for _, obj := range objs {
		seen[identityKey(obj)] = true
	}
	out := append([]*unstructured.Unstructured(nil), objs...)
	for _, obj := range objs {
		if !isInstaller(obj) {
			continue
		}
		declared, err := installerOutputs(obj)
		if err != nil {
			return nil, err
		}
		for _, d := range declared {
			k := identityKey(d)
			if seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, d)
		}
	}
	sortByApplyRank(out)
	return out, nil
}

// identityKey is the dedup identity for expandWithOutputs — kind,
// namespace, and name, matching the granularity Get/Delete address.
func identityKey(obj *unstructured.Unstructured) string {
	return obj.GetKind() + "|" + obj.GetNamespace() + "|" + obj.GetName()
}
