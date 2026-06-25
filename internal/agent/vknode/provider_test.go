package vknode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// fakeLibpod is a stateful fake of just the endpoints Provider hits.
// It tracks containers in-memory by ID so a test can assert
// post-conditions (e.g. "DeletePod must remove the container").
type fakeLibpod struct {
	mu         sync.Mutex
	containers map[string]*fakeContainer
	pulledRefs []string
}

type fakeContainer struct {
	ID       string
	Name     string
	Image    string
	Labels   map[string]string
	State    string // "created" | "running" | "exited"
	ExitCode int32
}

func newFakeLibpod() *fakeLibpod {
	return &fakeLibpod{containers: map[string]*fakeContainer{}}
}

func (f *fakeLibpod) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		// Strip the versioned prefix the client now sends
		// (/v5.0.0/libpod/...) so the rest of the switch can match
		// on the canonical /libpod/... shape. Real podman accepts
		// both forms; the fake mirrors that.
		path := strings.TrimPrefix(r.URL.Path, "/v5.0.0")
		switch {
		case path == "/libpod/_ping":
			_, _ = io.WriteString(w, "OK\n")
		case path == "/libpod/images/pull" && r.Method == http.MethodPost:
			ref := r.URL.Query().Get("reference")
			f.pulledRefs = append(f.pulledRefs, ref)
			_, _ = io.WriteString(w, `{"stream":"pulled"}`+"\n")
		case path == "/libpod/containers/create" && r.Method == http.MethodPost:
			var spec SpecGenerator
			if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			id := "id-" + spec.Name
			f.containers[id] = &fakeContainer{
				ID:     id,
				Name:   spec.Name,
				Image:  spec.Image,
				Labels: spec.Labels,
				State:  "created",
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"Id":"`+id+`","Warnings":[]}`)
		case path == "/libpod/containers/json" && r.Method == http.MethodGet:
			filt := r.URL.Query().Get("filters")
			var want map[string][]string
			_ = json.Unmarshal([]byte(filt), &want)
			out := []ListContainerItem{}
			for _, c := range f.containers {
				if !labelsMatch(c.Labels, want["label"]) {
					continue
				}
				out = append(out, ListContainerItem{
					ID:     c.ID,
					Names:  []string{"/" + c.Name},
					Image:  c.Image,
					State:  c.State,
					Labels: c.Labels,
				})
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(out)
		case strings.HasSuffix(path, "/start") && r.Method == http.MethodPost:
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/libpod/containers/"), "/start")
			c, ok := f.containers[id]
			if !ok {
				http.Error(w, "no such container", http.StatusNotFound)
				return
			}
			if c.State == "running" {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			c.State = "running"
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(path, "/json") && r.Method == http.MethodGet:
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/libpod/containers/"), "/json")
			c, ok := f.containers[id]
			if !ok {
				http.Error(w, "no such container", http.StatusNotFound)
				return
			}
			ins := InspectContainer{
				ID:        c.ID,
				Name:      c.Name,
				ImageName: c.Image,
				Image:     "sha256:fake-" + c.Image,
				Config:    InspectConfig{Labels: c.Labels},
			}
			ins.State.Status = c.State
			ins.State.Running = (c.State == "running")
			ins.State.ExitCode = c.ExitCode
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(ins)
		case r.Method == http.MethodDelete && strings.HasPrefix(path, "/libpod/containers/"):
			id := strings.TrimPrefix(path, "/libpod/containers/")
			if _, ok := f.containers[id]; !ok {
				http.Error(w, "no such container", http.StatusNotFound)
				return
			}
			delete(f.containers, id)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Logf("fakeLibpod: unhandled %s %s", r.Method, path)
			http.Error(w, "unhandled", http.StatusNotImplemented)
		}
	}
}

// labelsMatch reports whether haystack carries every "key=value" entry
// in needles. Used to simulate libpod's `filters={"label":[...]}` semantics.
func labelsMatch(haystack map[string]string, needles []string) bool {
	for _, n := range needles {
		k, v, hasEq := strings.Cut(n, "=")
		got, ok := haystack[k]
		if !ok {
			return false
		}
		if hasEq && got != v {
			return false
		}
	}
	return true
}

func newProviderWithFake(t *testing.T) (*Provider, *fakeLibpod) {
	t.Helper()
	fake := newFakeLibpod()
	sock := startFakeLibpod(t, fake.handler(t))
	p, err := NewProvider(sock)
	if err != nil {
		t.Fatal(err)
	}
	return p, fake
}

func newTestPod(name, uid string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "user-1",
			UID:       types.UID(uid),
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "main",
				Image: "docker.io/library/alpine:3.20",
			}},
		},
	}
}

func TestProvider_CreatePod_CreatesAndStarts(t *testing.T) {
	p, fake := newProviderWithFake(t)
	pod := newTestPod("hello", "11111111-aaaa-bbbb-cccc-dddddddddddd")

	if err := p.CreatePod(context.Background(), pod); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.containers) != 1 {
		t.Fatalf("want 1 container, got %d: %+v", len(fake.containers), fake.containers)
	}
	for _, c := range fake.containers {
		if c.State != "running" {
			t.Errorf("container not started: state=%q", c.State)
		}
		if c.Labels[PodUIDLabel] != string(pod.UID) {
			t.Errorf("PodUIDLabel missing: %+v", c.Labels)
		}
	}
	if len(fake.pulledRefs) != 1 || fake.pulledRefs[0] != "docker.io/library/alpine:3.20" {
		t.Errorf("pulledRefs: %+v", fake.pulledRefs)
	}
}

func TestProvider_CreatePod_AdoptsExistingContainer(t *testing.T) {
	p, fake := newProviderWithFake(t)
	pod := newTestPod("hello", "uid-1234-existing")

	// Pre-seed a "container we already own from a prior life", in the
	// stopped state. CreatePod must restart it without pulling again
	// or creating a duplicate.
	fake.containers["pre-existing-id"] = &fakeContainer{
		ID:    "pre-existing-id",
		Name:  ContainerName(pod),
		Image: pod.Spec.Containers[0].Image,
		Labels: map[string]string{
			ManagedLabel:      "true",
			PodUIDLabel:       string(pod.UID),
			PodNamespaceLabel: pod.Namespace,
			PodNameLabel:      pod.Name,
		},
		State: "exited",
	}

	if err := p.CreatePod(context.Background(), pod); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.containers) != 1 {
		t.Errorf("expected adoption, got duplicates: %+v", fake.containers)
	}
	if fake.containers["pre-existing-id"].State != "running" {
		t.Errorf("adopted container not restarted: %+v", fake.containers["pre-existing-id"])
	}
	if len(fake.pulledRefs) != 0 {
		t.Errorf("should not have pulled image on adoption: %+v", fake.pulledRefs)
	}
}

func TestProvider_GetPodStatus_Running(t *testing.T) {
	p, _ := newProviderWithFake(t)
	pod := newTestPod("hello", "uid-status-run")
	if err := p.CreatePod(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	status, err := p.GetPodStatus(context.Background(), pod.Namespace, pod.Name)
	if err != nil {
		t.Fatal(err)
	}
	if status.Phase != corev1.PodRunning {
		t.Errorf("phase: %q want Running", status.Phase)
	}
	if len(status.ContainerStatuses) != 1 || status.ContainerStatuses[0].State.Running == nil {
		t.Errorf("containerStatuses: %+v", status.ContainerStatuses)
	}
}

func TestProvider_GetPodStatus_Terminated(t *testing.T) {
	p, fake := newProviderWithFake(t)
	pod := newTestPod("hello", "uid-status-term")
	if err := p.CreatePod(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	// Flip the container to exited(1) behind the provider's back.
	fake.mu.Lock()
	for _, c := range fake.containers {
		c.State = "exited"
		c.ExitCode = 1
	}
	fake.mu.Unlock()

	status, err := p.GetPodStatus(context.Background(), pod.Namespace, pod.Name)
	if err != nil {
		t.Fatal(err)
	}
	if status.Phase != corev1.PodFailed {
		t.Errorf("phase: %q want Failed (exit 1)", status.Phase)
	}
	cs := status.ContainerStatuses[0]
	if cs.State.Terminated == nil || cs.State.Terminated.ExitCode != 1 {
		t.Errorf("terminated state: %+v", cs.State)
	}
	if cs.State.Terminated.Reason != "Error" {
		t.Errorf("reason: %q want Error", cs.State.Terminated.Reason)
	}
}

func TestProvider_GetPodStatus_ContainerMissing(t *testing.T) {
	p, fake := newProviderWithFake(t)
	pod := newTestPod("hello", "uid-missing")
	if err := p.CreatePod(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	// User manually rm'd the container.
	fake.mu.Lock()
	for k := range fake.containers {
		delete(fake.containers, k)
	}
	fake.mu.Unlock()

	status, err := p.GetPodStatus(context.Background(), pod.Namespace, pod.Name)
	if err != nil {
		t.Fatal(err)
	}
	if status.Phase != corev1.PodPending || status.Reason != "ContainerMissing" {
		t.Errorf("missing-container status: %+v", status)
	}
}

func TestProvider_DeletePod_Idempotent(t *testing.T) {
	p, fake := newProviderWithFake(t)
	pod := newTestPod("hello", "uid-delete")
	if err := p.CreatePod(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	if err := p.DeletePod(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	if len(fake.containers) != 0 {
		t.Errorf("container survived delete: %+v", fake.containers)
	}
	// Second delete: container already gone, must still succeed.
	if err := p.DeletePod(context.Background(), pod); err != nil {
		t.Errorf("idempotent delete failed: %v", err)
	}
}

func TestProvider_GetPod_NotFound(t *testing.T) {
	p, _ := newProviderWithFake(t)
	_, err := p.GetPod(context.Background(), "user-1", "never-created")
	if err == nil {
		t.Fatal("expected error")
	}
	if _, ok := err.(errNotFound); !ok {
		t.Fatalf("want errNotFound, got %T: %v", err, err)
	}
}

func TestProvider_GetPods_ReturnsCachedSlice(t *testing.T) {
	p, _ := newProviderWithFake(t)
	for i, name := range []string{"a", "b", "c"} {
		pod := newTestPod(name, "uid-multi-"+string(rune('0'+i)))
		if err := p.CreatePod(context.Background(), pod); err != nil {
			t.Fatal(err)
		}
	}
	pods, err := p.GetPods(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(pods) != 3 {
		t.Errorf("want 3 pods, got %d", len(pods))
	}
}

func TestProvider_Reconcile_RebuildsCacheFromLabels(t *testing.T) {
	p, fake := newProviderWithFake(t)
	// Pre-seed two containers we'd "created previously" plus one
	// outside container that lacks ManagedLabel — Reconcile must
	// ignore the latter.
	fake.containers["c1"] = &fakeContainer{
		ID: "c1", Name: "outpost-aaaaaaaa-main", Image: "alpine",
		Labels: map[string]string{
			ManagedLabel:       "true",
			PodUIDLabel:        "uid-a",
			PodNamespaceLabel:  "user-1",
			PodNameLabel:       "pod-a",
			ContainerNameLabel: "main",
		},
		State: "running",
	}
	fake.containers["c2"] = &fakeContainer{
		ID: "c2", Name: "outpost-bbbbbbbb-main", Image: "alpine",
		Labels: map[string]string{
			ManagedLabel:       "true",
			PodUIDLabel:        "uid-b",
			PodNamespaceLabel:  "user-2",
			PodNameLabel:       "pod-b",
			ContainerNameLabel: "main",
		},
		State: "exited",
	}
	fake.containers["unmanaged"] = &fakeContainer{
		ID: "unmanaged", Name: "users-own-container", Image: "nginx",
		// no ManagedLabel
		State: "running",
	}

	if err := p.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	pods, _ := p.GetPods(context.Background())
	if len(pods) != 2 {
		t.Fatalf("expected 2 reconciled pods, got %d", len(pods))
	}
	names := map[string]bool{}
	for _, p := range pods {
		names[p.Namespace+"/"+p.Name] = true
	}
	if !names["user-1/pod-a"] || !names["user-2/pod-b"] {
		t.Errorf("missing reconciled pods: %+v", names)
	}
}

func TestProvider_CreatePod_RejectsUnauthorizedNamespace(t *testing.T) {
	p, fake := newProviderWithFake(t)
	p.SetAccess(NewAccess("user-allowed-only"))

	pod := newTestPod("hello", "uid-denied")
	pod.Namespace = "user-someone-else"

	err := p.CreatePod(context.Background(), pod)
	if err == nil {
		t.Fatal("expected CreatePod to reject unauthorized namespace")
	}
	if !strings.Contains(err.Error(), "not permitted to schedule") {
		t.Errorf("error should explain the rejection: %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.containers) != 0 {
		t.Errorf("no container should have been created on rejection: %+v", fake.containers)
	}
}

func TestProvider_CreatePod_AcceptsAllowedNamespace(t *testing.T) {
	p, fake := newProviderWithFake(t)
	p.SetAccess(NewAccess("user-mine"))

	pod := newTestPod("hello", "uid-allowed")
	pod.Namespace = "user-mine"

	if err := p.CreatePod(context.Background(), pod); err != nil {
		t.Fatalf("CreatePod should accept allowed namespace; got %v", err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.containers) != 1 {
		t.Errorf("expected 1 container; got %+v", fake.containers)
	}
}

func TestProvider_CreatePod_NilAccessAcceptsAnything(t *testing.T) {
	// Sanity check the dev/single-tenant escape hatch: a Provider
	// without SetAccess accepts every namespace.
	p, _ := newProviderWithFake(t)
	pod := newTestPod("hello", "uid-nilaccess")
	pod.Namespace = "literally-any-namespace"
	if err := p.CreatePod(context.Background(), pod); err != nil {
		t.Fatalf("nil-Access Provider should accept any namespace; got %v", err)
	}
}

func TestProvider_UpdatePod_RefreshesCache(t *testing.T) {
	p, _ := newProviderWithFake(t)
	pod := newTestPod("hello", "uid-update")
	if err := p.CreatePod(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	// Change a label on the apiserver-side pod; UpdatePod must reflect it.
	pod.Labels = map[string]string{"v": "2"}
	if err := p.UpdatePod(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	got, err := p.GetPod(context.Background(), pod.Namespace, pod.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got.Labels["v"] != "2" {
		t.Errorf("UpdatePod did not refresh cache: %+v", got.Labels)
	}
}
