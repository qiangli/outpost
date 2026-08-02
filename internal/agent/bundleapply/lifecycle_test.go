package bundleapply

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// installerJob builds a headlamp-shaped installer Job: annotated as a
// lifecycle installer declaring a namespaced Deployment and a
// cluster-scoped ClusterRoleBinding as its durable outputs.
func installerJob() *unstructured.Unstructured {
	job := newObj("batch/v1", "Job", "headlamp", "install-headlamp")
	job.SetAnnotations(map[string]string{
		AnnotationLifecycle: LifecycleInstaller,
		AnnotationInstalls: "apps/v1:Deployment:headlamp:headlamp," +
			"rbac.authorization.k8s.io/v1:ClusterRoleBinding::headlamp-admin",
	})
	return job
}

// installerBundle is the surrounding bundle: Namespace + ServiceAccount
// (the bootstrap skeleton that survives even a failed install) plus the
// installer Job.
func installerBundle() *Bundle {
	objs := []*unstructured.Unstructured{
		newObj("v1", "Namespace", "", "headlamp"),
		newObj("v1", "ServiceAccount", "headlamp", "headlamp"),
		installerJob(),
	}
	sortByApplyRank(objs)
	return &Bundle{Objects: objs}
}

// seed applies objs into the fake so they exist as live state.
func seed(t *testing.T, fc *fakeClient, objs ...*unstructured.Unstructured) {
	t.Helper()
	for _, obj := range objs {
		if _, err := fc.Apply(context.Background(), obj); err != nil {
			t.Fatalf("seed %s: %v", identity(obj), err)
		}
	}
}

func findStatus(t *testing.T, objs []ObjectStatus, kind, name string) ObjectStatus {
	t.Helper()
	for _, o := range objs {
		if o.Kind == kind && o.Name == name {
			return o
		}
	}
	t.Fatalf("no status row for %s %s in %+v", kind, name, objs)
	return ObjectStatus{}
}

// The regression this contract exists for: the installer Job completed
// and was garbage-collected (ttlSecondsAfterFinished), the product is
// Running — status must report installed, not count the reaped Job as an
// absent object.
func TestStatusBundle_InstallerCompletedAndTTLDeleted(t *testing.T) {
	fc := newFakeClient()
	crb := newObj("rbac.authorization.k8s.io/v1", "ClusterRoleBinding", "", "headlamp-admin")
	seed(t, fc,
		newObj("v1", "Namespace", "", "headlamp"),
		newObj("v1", "ServiceAccount", "headlamp", "headlamp"),
		readyDeployment("headlamp", "headlamp"),
		crb,
	) // note: the Job itself is NOT in the store — TTL reaped it.

	res, err := StatusBundle(context.Background(), installerBundle(), StatusOptions{Client: fc})
	if err != nil {
		t.Fatalf("StatusBundle: %v", err)
	}
	if !res.Installed || !res.AllReady {
		t.Fatalf("TTL-reaped installer with all declared outputs present must report installed+ready: %+v", res)
	}
	job := findStatus(t, res.Objects, "Job", "install-headlamp")
	if job.Exists || !job.Installer {
		t.Fatalf("installer row must report absent + installer-marked: %+v", job)
	}
	if !strings.Contains(job.Reason, "garbage-collected") {
		t.Fatalf("installer reason should explain the TTL case: %q", job.Reason)
	}
	dep := findStatus(t, res.Objects, "Deployment", "headlamp")
	if !dep.DeclaredOutput || !dep.Exists || !dep.Ready {
		t.Fatalf("declared output Deployment must be reported present+ready: %+v", dep)
	}
	out := findStatus(t, res.Objects, "ClusterRoleBinding", "headlamp-admin")
	if !out.DeclaredOutput || !out.Exists {
		t.Fatalf("cluster-scoped declared output must be reported: %+v", out)
	}
}

// Fail closed: the Namespace/ServiceAccount skeleton survived, but the
// installer never completed (never ran, or failed and was reaped) — the
// declared outputs are missing, so installed must be false. This is the
// "never return installed=true merely because Namespace/RBAC survived a
// failed install" guarantee.
func TestStatusBundle_InstallerNeverCompleted(t *testing.T) {
	fc := newFakeClient()
	seed(t, fc,
		newObj("v1", "Namespace", "", "headlamp"),
		newObj("v1", "ServiceAccount", "headlamp", "headlamp"),
	) // no Job, no outputs — only the bootstrap skeleton.

	res, err := StatusBundle(context.Background(), installerBundle(), StatusOptions{Client: fc})
	if err != nil {
		t.Fatalf("StatusBundle: %v", err)
	}
	if res.Installed || res.AllReady {
		t.Fatalf("missing declared outputs must fail closed to not-installed: %+v", res)
	}
	job := findStatus(t, res.Objects, "Job", "install-headlamp")
	if !strings.Contains(job.Reason, "never completed") {
		t.Fatalf("installer reason should explain the fail-closed verdict: %q", job.Reason)
	}
}

// A failed installer Job still present on the cluster: judged by its own
// (terminal) readiness AND by the missing outputs — never installed.
func TestStatusBundle_InstallerFailedStillPresent(t *testing.T) {
	fc := newFakeClient()
	failed := installerJob()
	setNested(failed, []any{map[string]any{"type": "Failed", "status": "True"}}, "status", "conditions")
	seed(t, fc,
		newObj("v1", "Namespace", "", "headlamp"),
		newObj("v1", "ServiceAccount", "headlamp", "headlamp"),
		failed,
	)

	res, err := StatusBundle(context.Background(), installerBundle(), StatusOptions{Client: fc})
	if err != nil {
		t.Fatalf("StatusBundle: %v", err)
	}
	if res.Installed || res.AllReady {
		t.Fatalf("failed installer with missing outputs must not be installed: %+v", res)
	}
	job := findStatus(t, res.Objects, "Job", "install-headlamp")
	if !job.Exists || job.Ready {
		t.Fatalf("failed Job must report present but not ready: %+v", job)
	}
}

// Declared outputs exist but have not rolled out yet: installed (the
// product is there) but not all-ready — same bar the apply wait uses.
func TestStatusBundle_InstallerOutputsPresentNotReady(t *testing.T) {
	fc := newFakeClient()
	notReady := newObj("apps/v1", "Deployment", "headlamp", "headlamp")
	setNested(notReady, int64(1), "spec", "replicas")
	seed(t, fc,
		newObj("v1", "Namespace", "", "headlamp"),
		newObj("v1", "ServiceAccount", "headlamp", "headlamp"),
		notReady,
		newObj("rbac.authorization.k8s.io/v1", "ClusterRoleBinding", "", "headlamp-admin"),
	)

	res, err := StatusBundle(context.Background(), installerBundle(), StatusOptions{Client: fc})
	if err != nil {
		t.Fatalf("StatusBundle: %v", err)
	}
	if !res.Installed {
		t.Fatalf("all declared outputs present must report installed: %+v", res)
	}
	if res.AllReady {
		t.Fatalf("un-rolled-out declared output must hold all_ready false: %+v", res)
	}
}

// Uninstall removes the actual installed product — the declared outputs,
// including the cluster-scoped ClusterRoleBinding a Namespace deletion
// would never touch — not just the bootstrap manifest. Idempotent: the
// TTL-reaped Job (and any other already-gone object) deletes as success.
func TestDeleteBundle_RemovesDeclaredOutputs(t *testing.T) {
	fc := newFakeClient()
	seed(t, fc,
		newObj("v1", "Namespace", "", "headlamp"),
		newObj("v1", "ServiceAccount", "headlamp", "headlamp"),
		readyDeployment("headlamp", "headlamp"),
		newObj("rbac.authorization.k8s.io/v1", "ClusterRoleBinding", "", "headlamp-admin"),
	) // Job already TTL-reaped.

	res, err := DeleteBundle(context.Background(), installerBundle(), DeleteOptions{Client: fc})
	if err != nil {
		t.Fatalf("DeleteBundle: %v", err)
	}
	if len(res.Failed) != 0 {
		t.Fatalf("nothing should fail: %+v", res)
	}
	// 3 bundle objects + 2 declared outputs, all attempted.
	if len(res.Deleted) != 5 {
		t.Fatalf("expected 5 deletions (bundle + declared outputs), got %d: %v", len(res.Deleted), res.Deleted)
	}
	if len(fc.store) != 0 {
		t.Fatalf("store should be empty after uninstall, still holds: %v", fc.store)
	}
	// Dependency order: the Deployment output and the ClusterRoleBinding
	// output must be deleted before the Namespace (reverse apply rank).
	pos := map[string]int{}
	for i, k := range fc.deleteOrder {
		pos[k] = i
	}
	nsPos, ok := pos["Namespace||headlamp"]
	if !ok {
		t.Fatalf("Namespace not deleted: %v", fc.deleteOrder)
	}
	for _, k := range []string{"Deployment|headlamp|headlamp", "ClusterRoleBinding||headlamp-admin"} {
		p, ok := pos[k]
		if !ok {
			t.Fatalf("declared output %s not deleted: %v", k, fc.deleteOrder)
		}
		if p > nsPos {
			t.Fatalf("declared output %s deleted after its Namespace: %v", k, fc.deleteOrder)
		}
	}

	// Idempotent: a second uninstall over the now-empty cluster succeeds.
	res, err = DeleteBundle(context.Background(), installerBundle(), DeleteOptions{Client: fc})
	if err != nil || len(res.Failed) != 0 {
		t.Fatalf("re-run must converge cleanly: %v %+v", err, res)
	}
}

// A declared output that is ALSO a bundle object is deleted exactly once.
func TestDeleteBundle_DedupesOutputsAgainstBundle(t *testing.T) {
	fc := newFakeClient()
	dep := readyDeployment("headlamp", "headlamp")
	job := newObj("batch/v1", "Job", "headlamp", "install-headlamp")
	job.SetAnnotations(map[string]string{
		AnnotationLifecycle: LifecycleInstaller,
		AnnotationInstalls:  "apps/v1:Deployment:headlamp:headlamp",
	})
	objs := []*unstructured.Unstructured{newObj("v1", "Namespace", "", "headlamp"), dep, job}
	sortByApplyRank(objs)
	b := &Bundle{Objects: objs}
	seed(t, fc, objs...)

	res, err := DeleteBundle(context.Background(), b, DeleteOptions{Client: fc})
	if err != nil {
		t.Fatalf("DeleteBundle: %v", err)
	}
	if len(res.Deleted) != 3 {
		t.Fatalf("output shared with the bundle must delete once, got %d: %v", len(res.Deleted), res.Deleted)
	}
}

// The contract is enforced at load time, fail closed: an installer
// without a declaration — or with a malformed one, or an unknown
// lifecycle value — refuses to load at all.
func TestLoadBundle_EnforcesInstallerContract(t *testing.T) {
	write := func(t *testing.T, doc string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "install.yaml")
		if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	const ok = `apiVersion: batch/v1
kind: Job
metadata:
  name: install-headlamp
  namespace: headlamp
  annotations:
    outpost.dhnt.io/lifecycle: installer
    outpost.dhnt.io/installs: "apps/v1:Deployment:headlamp:headlamp"
`
	b, err := LoadBundle(write(t, ok))
	if err != nil {
		t.Fatalf("well-formed installer must load: %v", err)
	}
	if len(b.Objects) != 1 || !isInstaller(b.Objects[0]) {
		t.Fatalf("installer annotation lost in load: %+v", b.Objects)
	}

	const noOutputs = `apiVersion: batch/v1
kind: Job
metadata:
  name: install-headlamp
  namespace: headlamp
  annotations:
    outpost.dhnt.io/lifecycle: installer
`
	if _, err := LoadBundle(write(t, noOutputs)); err == nil || !strings.Contains(err.Error(), "declares no") {
		t.Fatalf("installer without outputs must refuse to load: %v", err)
	}

	const malformed = `apiVersion: batch/v1
kind: Job
metadata:
  name: install-headlamp
  namespace: headlamp
  annotations:
    outpost.dhnt.io/lifecycle: installer
    outpost.dhnt.io/installs: "Deployment/headlamp"
`
	if _, err := LoadBundle(write(t, malformed)); err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("malformed outputs entry must refuse to load: %v", err)
	}

	const unknown = `apiVersion: batch/v1
kind: Job
metadata:
  name: install-headlamp
  namespace: headlamp
  annotations:
    outpost.dhnt.io/lifecycle: cleanup
`
	if _, err := LoadBundle(write(t, unknown)); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unknown lifecycle value must refuse to load: %v", err)
	}
}

// Cluster-scoped entries use an empty namespace segment; both shapes
// parse into stubs Get/Delete can address.
func TestInstallerOutputs_Parsing(t *testing.T) {
	job := installerJob()
	outs, err := installerOutputs(job)
	if err != nil {
		t.Fatalf("installerOutputs: %v", err)
	}
	if len(outs) != 2 {
		t.Fatalf("expected 2 outputs, got %d", len(outs))
	}
	if outs[0].GetAPIVersion() != "apps/v1" || outs[0].GetKind() != "Deployment" ||
		outs[0].GetNamespace() != "headlamp" || outs[0].GetName() != "headlamp" {
		t.Fatalf("namespaced output parsed wrong: %+v", outs[0])
	}
	if outs[1].GetKind() != "ClusterRoleBinding" || outs[1].GetNamespace() != "" || outs[1].GetName() != "headlamp-admin" {
		t.Fatalf("cluster-scoped output parsed wrong: %+v", outs[1])
	}
}
