package vknode

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/rest"

	corev1 "k8s.io/api/core/v1"
)

// ---------------------------------------------------------------------------
// Cluster-identity derivation
// ---------------------------------------------------------------------------

// pemCert wraps arbitrary DER-ish bytes in a CERTIFICATE PEM block. The
// identity hash covers block.Bytes without parsing an actual x509
// certificate, so fixture bytes are fine.
func pemCert(der string) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte(der)})
}

func TestClusterIdentityFromCA(t *testing.T) {
	caA := pemCert("cluster-a-root")
	caB := pemCert("cluster-b-root")

	idA := ClusterIdentityFromCA(caA)
	idB := ClusterIdentityFromCA(caB)
	if idA == "" || idB == "" {
		t.Fatalf("identities must be non-empty: A=%q B=%q", idA, idB)
	}
	if idA == idB {
		t.Fatalf("distinct CAs must yield distinct identities: %q", idA)
	}
	if !strings.HasPrefix(idA, "ca256-") {
		t.Errorf("identity shape: %q", idA)
	}
	if got := ClusterIdentityFromCA(caA); got != idA {
		t.Errorf("not deterministic: %q vs %q", got, idA)
	}

	// PEM cosmetic differences (leading comment text, surrounding
	// whitespace) must not change the identity — the hash covers DER.
	dressed := append([]byte("# k3s root CA\n\n"), caA...)
	dressed = append(dressed, '\n', '\n')
	if got := ClusterIdentityFromCA(dressed); got != idA {
		t.Errorf("PEM re-dressing changed identity: %q vs %q", got, idA)
	}

	// Empty / whitespace-only → unscoped.
	if got := ClusterIdentityFromCA(nil); got != "" {
		t.Errorf("nil CA: %q", got)
	}
	if got := ClusterIdentityFromCA([]byte("  \n\t")); got != "" {
		t.Errorf("whitespace CA: %q", got)
	}

	// Non-PEM bytes still hash to a stable identity (raw-bytes fallback).
	raw1 := ClusterIdentityFromCA([]byte("not pem at all"))
	raw2 := ClusterIdentityFromCA([]byte("not pem at all"))
	if raw1 == "" || raw1 != raw2 {
		t.Errorf("raw fallback unstable: %q vs %q", raw1, raw2)
	}
}

func TestClusterIdentityFromRestConfig(t *testing.T) {
	ca := pemCert("some-root")
	want := ClusterIdentityFromCA(ca)

	// Inline CAData (the supervised-daemon path via ConfigFromCluster).
	cfg := &rest.Config{Host: "https://10.0.0.1:6443"}
	cfg.TLSClientConfig.CAData = ca
	got, err := ClusterIdentityFromRestConfig(cfg)
	if err != nil || got != want {
		t.Fatalf("CAData: got %q err %v, want %q", got, err, want)
	}

	// CAFile (the standalone kubeconfig path).
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(caPath, ca, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg2 := &rest.Config{Host: "https://other-address:6443"}
	cfg2.TLSClientConfig.CAFile = caPath
	got2, err := ClusterIdentityFromRestConfig(cfg2)
	if err != nil || got2 != want {
		t.Fatalf("CAFile: got %q err %v, want %q", got2, err, want)
	}
	// Same CA reached via a different API URL → same identity. That is
	// the point of fingerprinting the CA instead of the URL.
	if got != got2 {
		t.Errorf("identity depends on address: %q vs %q", got, got2)
	}

	// Unreadable CAFile fails closed instead of silently unscoping.
	cfg3 := &rest.Config{}
	cfg3.TLSClientConfig.CAFile = filepath.Join(dir, "missing.crt")
	if _, err := ClusterIdentityFromRestConfig(cfg3); err == nil {
		t.Error("missing CA file should error")
	}

	// No CA at all → unscoped, no error.
	if got, err := ClusterIdentityFromRestConfig(&rest.Config{}); err != nil || got != "" {
		t.Errorf("no CA: got %q err %v", got, err)
	}
	if got, err := ClusterIdentityFromRestConfig(nil); err != nil || got != "" {
		t.Errorf("nil cfg: got %q err %v", got, err)
	}
}

// ---------------------------------------------------------------------------
// Two clusters, one podman socket
// ---------------------------------------------------------------------------

var (
	clusterIDA = ClusterIdentityFromCA(pemCert("peer-dks-root-ca"))
	clusterIDB = ClusterIdentityFromCA(pemCert("cloud-dks-root-ca"))
)

// twoClusterProviders stands up ONE fake libpod socket and two scoped
// Providers driving it — the live-probe topology: a standalone
// vk-podman on peer DKS plus the supervised daemon's provider on cloud
// DKS, sharing the host's podman daemon.
func twoClusterProviders(t *testing.T) (pA, pB *Provider, fake *fakeLibpod) {
	t.Helper()
	fake = newFakeLibpod()
	sock := startFakeLibpod(t, fake.handler(t))
	pA, err := NewProvider(sock, clusterIDA)
	if err != nil {
		t.Fatal(err)
	}
	pB, err = NewProvider(sock, clusterIDB)
	if err != nil {
		t.Fatal(err)
	}
	return pA, pB, fake
}

// seedLegacyContainer plants a pre-scoping managed container (no
// ClusterLabel) exactly as an old outpost build would have created it.
func seedLegacyContainer(fake *fakeLibpod, pod *corev1.Pod, state string) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.containers["legacy-"+string(pod.UID)] = &fakeContainer{
		ID:    "legacy-" + string(pod.UID),
		Name:  ContainerName(pod),
		Image: pod.Spec.Containers[0].Image,
		Labels: map[string]string{
			ManagedLabel:       "true",
			PodUIDLabel:        string(pod.UID),
			PodNamespaceLabel:  pod.Namespace,
			PodNameLabel:       pod.Name,
			ContainerNameLabel: pod.Spec.Containers[0].Name,
		},
		State: state,
	}
}

func TestTwoClusters_CreateStampsClusterLabel(t *testing.T) {
	pA, pB, fake := twoClusterProviders(t)

	podA := newTestPod("web", "aaaaaaaa-1111-4111-8111-111111111111")
	podB := newTestPod("db", "bbbbbbbb-2222-4222-8222-222222222222")
	if err := pA.CreatePod(context.Background(), podA); err != nil {
		t.Fatal(err)
	}
	if err := pB.CreatePod(context.Background(), podB); err != nil {
		t.Fatal(err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	owners := map[string]string{}
	for _, c := range fake.containers {
		owners[c.Labels[PodUIDLabel]] = c.Labels[ClusterLabel]
	}
	if owners[string(podA.UID)] != clusterIDA {
		t.Errorf("pod A owner label: %q want %q", owners[string(podA.UID)], clusterIDA)
	}
	if owners[string(podB.UID)] != clusterIDB {
		t.Errorf("pod B owner label: %q want %q", owners[string(podB.UID)], clusterIDB)
	}
}

// TestTwoClusters_ReconcileSeesOnlyOwnContainers is the incident
// regression: reconcile (backend List) feeding the PodController is a
// license to garbage-collect, so listing the OTHER cluster's container
// is what turned into the cross-delete. Each provider must see exactly
// its own containers — foreign and legacy ones stay invisible.
func TestTwoClusters_ReconcileSeesOnlyOwnContainers(t *testing.T) {
	pA, pB, fake := twoClusterProviders(t)
	ctx := context.Background()

	podA := newTestPod("web", "aaaaaaaa-1111-4111-8111-111111111111")
	podB := newTestPod("db", "bbbbbbbb-2222-4222-8222-222222222222")
	if err := pA.CreatePod(ctx, podA); err != nil {
		t.Fatal(err)
	}
	if err := pB.CreatePod(ctx, podB); err != nil {
		t.Fatal(err)
	}
	legacyPod := newTestPod("old", "cccccccc-3333-4333-8333-333333333333")
	seedLegacyContainer(fake, legacyPod, "running")

	// Fresh providers = daemon restart on both sides.
	fake.mu.Lock()
	sockContainers := len(fake.containers)
	fake.mu.Unlock()
	if sockContainers != 3 {
		t.Fatalf("fixture: want 3 containers, got %d", sockContainers)
	}

	for _, tc := range []struct {
		prov    *Provider
		wantPod *corev1.Pod
	}{{pA, podA}, {pB, podB}} {
		// Reset the pod cache the CreatePod above populated so Reconcile
		// is the only source of what this provider knows.
		tc.prov.mu.Lock()
		tc.prov.pods = map[string]*corev1.Pod{}
		tc.prov.mu.Unlock()

		if err := tc.prov.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		pods, err := tc.prov.GetPods(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(pods) != 1 || pods[0].Name != tc.wantPod.Name || string(pods[0].UID) != string(tc.wantPod.UID) {
			t.Errorf("reconcile leaked foreign/legacy pods: %+v", pods)
		}
	}

	// Reconcile must not have deleted anything.
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.containers) != 3 {
		t.Errorf("reconcile mutated the socket: %d containers left", len(fake.containers))
	}
}

// TestTwoClusters_NoCrossDelete drives the delete path directly with the
// other cluster's pod (same UID — the strongest possible confusion): the
// foreign-owned container must survive.
func TestTwoClusters_NoCrossDelete(t *testing.T) {
	pA, pB, fake := twoClusterProviders(t)
	ctx := context.Background()

	podA := newTestPod("web", "aaaaaaaa-1111-4111-8111-111111111111")
	if err := pA.CreatePod(ctx, podA); err != nil {
		t.Fatal(err)
	}

	// Cluster B is handed a pod with the SAME UID (synthetic — UIDs are
	// UUIDs — but this is the worst case for the ownership rule).
	if err := pB.DeletePod(ctx, podA.DeepCopy()); err != nil {
		t.Fatalf("foreign delete should no-op, not error: %v", err)
	}

	fake.mu.Lock()
	survivors := len(fake.containers)
	fake.mu.Unlock()
	if survivors != 1 {
		t.Fatalf("cluster B deleted cluster A's container")
	}

	// The rightful owner can still delete it.
	if err := pA.DeletePod(ctx, podA); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.containers) != 0 {
		t.Errorf("owner delete failed: %+v", fake.containers)
	}
}

// TestTwoClusters_NoCrossAdopt: cluster B asked to ensure a pod whose
// UID matches cluster A's container must NOT adopt (or restart) it. The
// deterministic UID-derived container name then collides at create —
// the loud failure we want instead of a silent takeover.
func TestTwoClusters_NoCrossAdopt(t *testing.T) {
	pA, pB, fake := twoClusterProviders(t)
	ctx := context.Background()

	podA := newTestPod("web", "aaaaaaaa-1111-4111-8111-111111111111")
	if err := pA.CreatePod(ctx, podA); err != nil {
		t.Fatal(err)
	}
	// Stop it so an (incorrect) adoption would be observable as a
	// restart.
	fake.mu.Lock()
	for _, c := range fake.containers {
		c.State = "exited"
	}
	fake.mu.Unlock()

	err := pB.CreatePod(ctx, podA.DeepCopy())
	if err == nil {
		t.Fatal("cross-cluster create with colliding UID must fail, not adopt")
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.containers) != 1 {
		t.Fatalf("container set changed: %+v", fake.containers)
	}
	for _, c := range fake.containers {
		if c.Labels[ClusterLabel] != clusterIDA {
			t.Errorf("ownership label rewritten: %q", c.Labels[ClusterLabel])
		}
		if c.State != "exited" {
			t.Errorf("cluster B restarted cluster A's container (adopted it): state=%q", c.State)
		}
	}
}

// TestLegacyContainer_FailClosedInReconcile_AdoptedOnUIDMatch encodes
// the migration rule: a pre-scoping container is ambiguous from
// reconcile (never listed → never GC'd, by either cluster), but an
// apiserver-driven CreatePod whose pod UID matches is unambiguous proof
// of ownership — adopt and, later, delete work.
func TestLegacyContainer_FailClosedInReconcile_AdoptedOnUIDMatch(t *testing.T) {
	pA, pB, fake := twoClusterProviders(t)
	ctx := context.Background()

	legacyPod := newTestPod("old", "cccccccc-3333-4333-8333-333333333333")
	seedLegacyContainer(fake, legacyPod, "exited")

	// Ambiguous from BOTH reconciles.
	for _, prov := range []*Provider{pA, pB} {
		if err := prov.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		if pods, _ := prov.GetPods(ctx); len(pods) != 0 {
			t.Fatalf("legacy container leaked into reconcile: %+v", pods)
		}
	}
	fake.mu.Lock()
	if len(fake.containers) != 1 {
		t.Fatalf("reconcile touched the legacy container")
	}
	fake.mu.Unlock()

	// UID-matched CreatePod adopts: restarted in place, no duplicate,
	// no pull.
	if err := pA.CreatePod(ctx, legacyPod); err != nil {
		t.Fatalf("UID-matched adopt: %v", err)
	}
	fake.mu.Lock()
	if len(fake.containers) != 1 {
		t.Fatalf("adopt duplicated the container: %+v", fake.containers)
	}
	if fake.containers["legacy-"+string(legacyPod.UID)].State != "running" {
		t.Errorf("adopted container not restarted")
	}
	if len(fake.pulledRefs) != 0 {
		t.Errorf("adopt should not pull: %+v", fake.pulledRefs)
	}
	fake.mu.Unlock()

	// And the UID-matched delete reaps it.
	if err := pA.DeletePod(ctx, legacyPod); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.containers) != 0 {
		t.Errorf("UID-matched legacy delete failed: %+v", fake.containers)
	}
}

// TestUnscopedProvider_NeverTouchesScopedContainers covers the other
// migration direction: an old (unscoped, clusterID == "") daemon on the
// same socket must not list, adopt, or delete containers a scoped
// provider has claimed — while still owning its own unlabeled ones.
func TestUnscopedProvider_NeverTouchesScopedContainers(t *testing.T) {
	fake := newFakeLibpod()
	sock := startFakeLibpod(t, fake.handler(t))
	scoped, err := NewProvider(sock, clusterIDA)
	if err != nil {
		t.Fatal(err)
	}
	unscoped, err := NewProvider(sock, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	podScoped := newTestPod("web", "aaaaaaaa-1111-4111-8111-111111111111")
	if err := scoped.CreatePod(ctx, podScoped); err != nil {
		t.Fatal(err)
	}
	legacyPod := newTestPod("old", "cccccccc-3333-4333-8333-333333333333")
	seedLegacyContainer(fake, legacyPod, "running")

	if err := unscoped.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	pods, _ := unscoped.GetPods(ctx)
	if len(pods) != 1 || string(pods[0].UID) != string(legacyPod.UID) {
		t.Fatalf("unscoped provider must list exactly its legacy container: %+v", pods)
	}

	// Even a UID match must not let the unscoped provider claim a
	// scoped container.
	if err := unscoped.DeletePod(ctx, podScoped.DeepCopy()); err != nil {
		t.Fatalf("foreign delete should no-op: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.containers) != 2 {
		t.Errorf("unscoped provider deleted a scoped container: %+v", fake.containers)
	}
}

// ---------------------------------------------------------------------------
// Volume ownership
// ---------------------------------------------------------------------------

func TestHostPathVolumeName_ClusterScoped(t *testing.T) {
	legacy := hostPathVolumeName("", "user-1", "/srv/data")
	a := hostPathVolumeName(clusterIDA, "user-1", "/srv/data")
	b := hostPathVolumeName(clusterIDB, "user-1", "/srv/data")
	if a == b {
		t.Error("two clusters share a hostPath volume name")
	}
	if a == legacy || b == legacy {
		t.Error("scoped name collides with the legacy name")
	}
	// Legacy naming stays byte-stable so pre-scoping deployments keep
	// their volumes: the unscoped formula must not have changed shape.
	if legacy != hostPathVolumeName("", "user-1", "/srv/data") {
		t.Error("legacy naming not deterministic")
	}
	for _, n := range []string{legacy, a, b} {
		if !strings.HasPrefix(n, "outpost-hp-") {
			t.Errorf("name shape: %q", n)
		}
	}
}

func TestEnsureVolumesForPod_StampsClusterLabel(t *testing.T) {
	type volCreate struct {
		Name   string            `json:"Name"`
		Labels map[string]string `json:"Labels"`
	}
	var created []volCreate
	sock := startFakeLibpod(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/libpod/volumes/create") {
			http.Error(w, "unexpected", http.StatusNotImplemented)
			return
		}
		var vc volCreate
		if err := json.NewDecoder(r.Body).Decode(&vc); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		created = append(created, vc)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	})
	c, err := NewClient(sock)
	if err != nil {
		t.Fatal(err)
	}

	pod := newTestPod("web", "aaaaaaaa-1111-4111-8111-111111111111")
	pod.Spec.Volumes = []corev1.Volume{
		{Name: "data", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/srv/data"}}},
		{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
	if err := EnsureVolumesForPod(context.Background(), c, pod, clusterIDA); err != nil {
		t.Fatal(err)
	}
	if len(created) != 2 {
		t.Fatalf("want 2 volume creates, got %+v", created)
	}
	for _, vc := range created {
		if vc.Labels[ClusterLabel] != clusterIDA {
			t.Errorf("volume %q missing cluster label: %+v", vc.Name, vc.Labels)
		}
	}
	if created[0].Name != hostPathVolumeName(clusterIDA, pod.Namespace, "/srv/data") {
		t.Errorf("hostPath volume name not cluster-scoped: %q", created[0].Name)
	}
}

// The container spec and the pre-created volumes must reference the SAME
// scoped hostPath name, and the spec must carry the cluster label —
// including when a workload tries to forge one via pod labels.
func TestBuildSpecForCluster_LabelsAndVolumeNames(t *testing.T) {
	pod := newTestPod("web", "aaaaaaaa-1111-4111-8111-111111111111")
	pod.Labels = map[string]string{ClusterLabel: "ca256-forged0000000"}
	pod.Spec.Volumes = []corev1.Volume{
		{Name: "data", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/srv/data"}}},
	}
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "data", MountPath: "/data"}}

	spec, err := BuildSpecForCluster(pod, clusterIDA)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Labels[ClusterLabel] != clusterIDA {
		t.Errorf("cluster label: %q (forgery must lose to the backend's identity)", spec.Labels[ClusterLabel])
	}
	if len(spec.Volumes) != 1 || spec.Volumes[0].Name != hostPathVolumeName(clusterIDA, pod.Namespace, "/srv/data") {
		t.Errorf("spec volume name not scoped: %+v", spec.Volumes)
	}

	// Unscoped BuildSpec keeps legacy behavior: no cluster label.
	legacySpec, err := BuildSpec(pod.DeepCopy())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := legacySpec.Labels[ClusterLabel]; ok {
		t.Errorf("unscoped spec must not carry a cluster label: %+v", legacySpec.Labels)
	}
}
