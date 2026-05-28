package vkpodman

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Provider implements virtual-kubelet's node.PodLifecycleHandler on top
// of a local podman daemon. It is the per-outpost half of the cluster:
// the cloud-side PodController watches the apiserver for Pods assigned
// to this node and calls into us; we translate to libpod and back.
//
// Concurrency: all exported methods are safe to call from multiple
// goroutines. The internal pod cache is protected by an RWMutex; libpod
// itself serializes container-state changes per-container, so we don't
// have to worry about racing CreateContainer+StartContainer against a
// concurrent RemoveContainer for the same pod (they would have to be
// distinct pods to begin with, since the pod UID makes container names
// unique).
type Provider struct {
	client *Client
	access *Access       // nil = no namespace check (single-tenant dev)
	apps   TransientApps // nil = don't publish pods into the outpost's app router

	mu   sync.RWMutex
	pods map[string]*corev1.Pod // namespace/name → cached Pod
}

// NewProvider returns a Provider that talks to the libpod daemon
// reachable at podmanSocket (typically the path returned by
// agent.DetectPodman). Call Reconcile once after construction to
// repopulate the in-memory pod cache from containers podman is already
// running with the outpost.io/managed label — this is what makes
// vkpodman survive a crash without forgetting what it owns.
func NewProvider(podmanSocket string) (*Provider, error) {
	c, err := NewClient(podmanSocket)
	if err != nil {
		return nil, err
	}
	return &Provider{
		client: c,
		pods:   make(map[string]*corev1.Pod),
	}, nil
}

// Client returns the underlying libpod client. Exported so the
// NodeProvider can share the same socket connection.
func (p *Provider) Client() *Client { return p.client }

// SetAccess installs the namespace-access gate. Pass nil to disable
// the check (dev/single-tenant mode). Called once at boot from
// startClusterRunner with an Access built from the outpost owner's
// email + any sharee emails fetched from cloudbox.
func (p *Provider) SetAccess(a *Access) { p.access = a }

// SetTransientApps installs the local app router each Running pod
// gets published into (one transient entry per Container port with
// a non-zero HostPort). The published name follows
// TransientAppName(...), so cloudbox's /api/cluster/svc/* handler
// can compose a /h/<node>/app/<name>/ URL without negotiating
// container-port mapping out of band. Pass nil to skip publishing —
// the cluster still works, just only reachable via direct hostPort
// on the node's LAN.
func (p *Provider) SetTransientApps(a TransientApps) { p.apps = a }

func podKey(namespace, name string) string { return namespace + "/" + name }

// CreatePod creates and starts the container for pod. Idempotent: if a
// container with the same PodUIDLabel already exists, we just (re)start
// it instead of erroring on name conflict. That makes the reconcile
// path — "we saw this pod before our last restart, libpod still has the
// container" — collapse to a no-op rather than a 409 cascade.
//
// First gate: the namespace access check. p.access (when non-nil) holds
// the set of namespaces permitted to schedule here — derived from the
// outpost's owner + sharees. Pods from outside that set are rejected
// with a clear error so the apiserver event surface shows what
// happened. nil p.access means "no check"; used in dev/single-tenant
// modes where the operator hasn't wired Access yet.
func (p *Provider) CreatePod(ctx context.Context, pod *corev1.Pod) error {
	if !p.access.Allowed(pod.Namespace) {
		slog.Warn("vkpodman: rejecting CreatePod for unauthorized namespace",
			"pod", podKey(pod.Namespace, pod.Name), "allowed", p.access.Snapshot())
		return fmt.Errorf("vkpodman: namespace %q is not permitted to schedule on this outpost", pod.Namespace)
	}
	// Allocate ports for any containerPort the Pod manifest left without
	// an explicit hostPort. Mutates the in-memory pod in place so the
	// later BuildSpec, the labels we stamp on the container, and the
	// transient app publish all agree on the resolved port set.
	if _, err := AllocateMissingHostPorts(pod); err != nil {
		return err
	}
	spec, err := BuildSpec(pod)
	if err != nil {
		return err
	}
	// Source-canonical build: if the pod carries build-source
	// annotations, ensure the target image is available locally
	// (either by building now or confirming it's already cached).
	// skipPull is true when EnsureImageBuilt produced or found a
	// local image — we then bypass the PullImage step below, since
	// a localhost-tagged image can't be resolved against any
	// external registry.
	skipPull, err := p.EnsureImageBuilt(ctx, pod)
	if err != nil {
		return fmt.Errorf("vkpodman: build for pod %s: %w", podKey(pod.Namespace, pod.Name), err)
	}

	// Look for an existing container that already belongs to this Pod
	// UID. If found, we own it from a prior incarnation — just make sure
	// it's running and cache the spec.
	existing, err := p.findContainerByPodUID(ctx, string(pod.UID))
	if err != nil {
		return fmt.Errorf("vkpodman: lookup existing container for pod %s: %w", podKey(pod.Namespace, pod.Name), err)
	}
	if existing != "" {
		// Container survived a prior daemon run — read the host-port
		// labels we stamped at original-create time so the in-memory
		// pod (and the subsequent publishPod) sees the SAME hostPort
		// the original allocation chose. Without this, daemon restart
		// can leave a Running container reachable on port X while
		// vkpodman's view still says HostPort=0 and the transient
		// AppRegistry entry never gets created.
		if ins, ierr := p.client.InspectContainer(ctx, existing); ierr == nil && ins != nil {
			HydratePodPortsFromLabels(pod, ins.Config.Labels)
		} else if ierr != nil {
			slog.Warn("vkpodman: inspect existing container for port hydration",
				"container", existing, "err", ierr)
		}
		if err := p.client.StartContainer(ctx, existing); err != nil && !IsConflict(err) {
			return fmt.Errorf("vkpodman: start existing container %s: %w", existing, err)
		}
		p.cachePod(pod)
		slog.Info("vkpodman: adopted existing container",
			"pod", podKey(pod.Namespace, pod.Name), "container", existing)
		publishPod(p.apps, pod)
		return nil
	}

	// First time we've seen this pod (or libpod lost the container).
	// Pull the image opportunistically — failures here are fatal because
	// the next create will just fail with "no such image". Skipped when
	// EnsureImageBuilt has confirmed the image is available locally
	// (build-source workloads have no upstream registry to pull from).
	if !skipPull {
		if err := p.client.PullImage(ctx, spec.Image); err != nil {
			return fmt.Errorf("vkpodman: pull image %q: %w", spec.Image, err)
		}
	}

	created, err := p.client.CreateContainer(ctx, spec)
	if err != nil {
		return fmt.Errorf("vkpodman: create container for pod %s: %w", podKey(pod.Namespace, pod.Name), err)
	}
	for _, w := range created.Warnings {
		slog.Warn("vkpodman: libpod create warning",
			"pod", podKey(pod.Namespace, pod.Name), "warning", w)
	}
	if err := p.client.StartContainer(ctx, created.ID); err != nil {
		return fmt.Errorf("vkpodman: start container %s: %w", created.ID, err)
	}
	p.cachePod(pod)
	slog.Info("vkpodman: created container",
		"pod", podKey(pod.Namespace, pod.Name), "container", created.ID, "image", spec.Image)
	publishPod(p.apps, pod)
	return nil
}

// UpdatePod handles spec/label/annotation changes from the apiserver.
// Pod containers are immutable in K8s, so the only thing we need to do
// is refresh the cached *corev1.Pod — the running container stays put.
// (Label-only updates that influence which selector matches a workload
// are an apiserver concern; the container is unaffected.)
func (p *Provider) UpdatePod(_ context.Context, pod *corev1.Pod) error {
	p.cachePod(pod)
	// Republish the transient app registration. UpdatePod is the
	// PodController's first call for each pod on daemon restart
	// (vkpodman.Reconcile's adopt path only builds a port-less
	// skeleton, so publishPod from CreatePod's adopt branch would
	// be a no-op there). With this, transient apps survive a daemon
	// restart even when libpod still owns the container.
	// AppRegistry.Register is idempotent (it overwrites entries),
	// so repeated UpdatePods for the same pod converge cleanly.
	publishPod(p.apps, pod)
	return nil
}

// DeletePod stops + removes the container and forgets the pod. Idempotent
// against the reconcile path: a missing container (already cleaned up
// by a prior delete that crashed mid-flight) is not an error.
func (p *Provider) DeletePod(ctx context.Context, pod *corev1.Pod) error {
	cid, err := p.findContainerByPodUID(ctx, string(pod.UID))
	if err != nil {
		return fmt.Errorf("vkpodman: lookup container for pod %s: %w", podKey(pod.Namespace, pod.Name), err)
	}
	if cid != "" {
		// Force=true → stop then remove in one call. Volumes=true →
		// drop anonymous tmpfs/emptyDir volumes; named volumes from a
		// future PVC implementation must stay.
		if err := p.client.RemoveContainer(ctx, cid, true, true); err != nil && !IsNotFound(err) {
			return fmt.Errorf("vkpodman: remove container %s: %w", cid, err)
		}
	}
	unpublishPod(p.apps, pod)
	p.forgetPod(pod.Namespace, pod.Name)
	slog.Info("vkpodman: deleted pod",
		"pod", podKey(pod.Namespace, pod.Name), "container", cid)
	return nil
}

// GetPod returns the cached *corev1.Pod for (namespace, name). Reports
// errNotFound when we have no record — the PodController treats that as
// "this provider doesn't know about this pod" and falls back to its own
// state.
func (p *Provider) GetPod(_ context.Context, namespace, name string) (*corev1.Pod, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	pod, ok := p.pods[podKey(namespace, name)]
	if !ok {
		return nil, errNotFound{Namespace: namespace, Name: name}
	}
	return pod.DeepCopy(), nil
}

// GetPodStatus reports the live status of the pod's single container by
// inspecting libpod. Falls back to a Pending status when no container
// exists yet (e.g. between CreatePod returning and the container fully
// starting) so the PodController doesn't see a transient "not found"
// and panic.
func (p *Provider) GetPodStatus(ctx context.Context, namespace, name string) (*corev1.PodStatus, error) {
	pod, err := p.GetPod(ctx, namespace, name)
	if err != nil {
		return nil, err
	}
	cid, err := p.findContainerByPodUID(ctx, string(pod.UID))
	if err != nil {
		return nil, err
	}
	if cid == "" {
		// Container vanished underneath us — surface as Pending so the
		// reconciler will call CreatePod next time around.
		return &corev1.PodStatus{
			Phase:  corev1.PodPending,
			Reason: "ContainerMissing",
		}, nil
	}
	inspect, err := p.client.InspectContainer(ctx, cid)
	if err != nil {
		return nil, err
	}
	return inspectToPodStatus(ctx, pod, inspect), nil
}

// GetPods returns every Pod we currently know about. Used by the
// PodController on startup to discover what we already own — combined
// with the apiserver's view, that's how the reconcile loop computes
// what to create/delete to converge.
func (p *Provider) GetPods(_ context.Context) ([]*corev1.Pod, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]*corev1.Pod, 0, len(p.pods))
	for _, pod := range p.pods {
		out = append(out, pod.DeepCopy())
	}
	return out, nil
}

// Reconcile rebuilds the in-memory pod cache from libpod's view of the
// world. Called once at startup so a vk restart doesn't lose track of
// containers we created in a previous lifetime. Containers that lack
// the ManagedLabel are left alone — they belong to the user, not to
// the cluster.
//
// We reconstruct skeleton *corev1.Pods from the labels we stamped at
// create time. The reconstruction is intentionally minimal (no env, no
// resource limits, etc.) because the apiserver is the source of truth
// for the full spec; the PodController will issue an UpdatePod with
// the real Pod as soon as it lists the apiserver, refreshing the cache.
func (p *Provider) Reconcile(ctx context.Context) error {
	items, err := p.client.ListContainers(ctx, true, map[string]string{ManagedLabel: "true"})
	if err != nil {
		return fmt.Errorf("vkpodman: list managed containers: %w", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, item := range items {
		ns := item.Labels[PodNamespaceLabel]
		name := item.Labels[PodNameLabel]
		uid := item.Labels[PodUIDLabel]
		cname := item.Labels[ContainerNameLabel]
		if ns == "" || name == "" || uid == "" {
			slog.Warn("vkpodman: managed container missing identity labels",
				"container", item.ID, "labels", item.Labels)
			continue
		}
		skeleton := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: ns,
				Name:      name,
				UID:       types.UID(uid),
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:  cname,
					Image: item.Image,
					Ports: portsFromLabels(item.Labels),
				}},
			},
		}
		p.pods[podKey(ns, name)] = skeleton
		// Republish the transient AppRegistry entry from the labels
		// stamped at original-create time. Without this, there's a
		// window after daemon restart where libpod containers are
		// running but cloudbox /api/cluster/svc/* responds with
		// "unknown app" until PodController's first UpdatePod fires
		// (seconds-to-minutes depending on apiserver responsiveness).
		// Idempotent — Register overwrites entries.
		publishPod(p.apps, skeleton)
	}
	slog.Info("vkpodman: reconcile complete", "containers", len(items), "pods_cached", len(p.pods))
	return nil
}

// findContainerByPodUID returns the libpod container ID for the given
// pod UID, or "" if no managed container matches. Errors only on
// list-level failures (network, etc.) — empty result is a normal "not
// here yet / already gone" signal.
func (p *Provider) findContainerByPodUID(ctx context.Context, podUID string) (string, error) {
	if podUID == "" {
		return "", nil
	}
	items, err := p.client.ListContainers(ctx, true, map[string]string{
		ManagedLabel: "true",
		PodUIDLabel:  podUID,
	})
	if err != nil {
		return "", err
	}
	if len(items) == 0 {
		return "", nil
	}
	if len(items) > 1 {
		// Shouldn't happen — ContainerName is deterministic per podUID.
		// Pick the first running one if any, else the first; log it so
		// we can investigate.
		slog.Warn("vkpodman: multiple containers match pod UID",
			"pod_uid", podUID, "count", len(items))
		for _, it := range items {
			if it.State == "running" {
				return it.ID, nil
			}
		}
	}
	return items[0].ID, nil
}

func (p *Provider) cachePod(pod *corev1.Pod) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pods[podKey(pod.Namespace, pod.Name)] = pod.DeepCopy()
}

func (p *Provider) forgetPod(namespace, name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.pods, podKey(namespace, name))
}

// errNotFound implements the errdefs.NotFound contract virtual-kubelet
// uses to recognize "this pod isn't here" without forcing callers to
// import the errdefs package directly. The PodController checks via
// errdefs.IsNotFound which uses an interface-type assertion, so any
// type with the right method works.
type errNotFound struct {
	Namespace string
	Name      string
}

func (e errNotFound) Error() string {
	return fmt.Sprintf("vkpodman: pod %s/%s not found", e.Namespace, e.Name)
}
func (e errNotFound) NotFound() {}

// ErrNotFound returns the not-found marker for tests and external
// callers that want to recognize it via errors.As / errors.Is.
var ErrNotFound = errNotFound{}

// inspectToPodStatus translates a libpod InspectContainer to a
// corev1.PodStatus. We model the single container as
// pod.Status.ContainerStatuses[0] and derive PodPhase from its state.
func inspectToPodStatus(ctx context.Context, pod *corev1.Pod, ins *InspectContainer) *corev1.PodStatus {
	cs := corev1.ContainerStatus{
		Name:        pod.Spec.Containers[0].Name,
		Image:       ins.ImageName,
		ImageID:     ins.Image,
		ContainerID: "podman://" + ins.ID,
		Ready:       ins.State.Running,
	}
	switch {
	case ins.State.Running:
		cs.State.Running = &corev1.ContainerStateRunning{
			StartedAt: metav1.NewTime(ins.State.StartedAt),
		}
	case ins.State.Status == "exited" || ins.State.Status == "stopped" || ins.State.Dead:
		t := &corev1.ContainerStateTerminated{
			ExitCode:    ins.State.ExitCode,
			Reason:      terminatedReason(ins.State),
			Message:     ins.State.Error,
			ContainerID: cs.ContainerID,
			StartedAt:   metav1.NewTime(ins.State.StartedAt),
			FinishedAt:  metav1.NewTime(ins.State.FinishedAt),
		}
		cs.State.Terminated = t
	default:
		cs.State.Waiting = &corev1.ContainerStateWaiting{
			Reason: waitingReason(ins.State),
		}
	}

	status := &corev1.PodStatus{
		Phase:             phaseFromContainer(cs),
		HostIP:            "",
		PodIP:             "",
		ContainerStatuses: []corev1.ContainerStatus{cs},
	}
	if !ins.State.StartedAt.IsZero() {
		t := metav1.NewTime(ins.State.StartedAt)
		status.StartTime = &t
	}
	// Conditions[Ready] reflects libpod state PLUS the container's
	// readinessProbe when one is declared. Without a probe, "running
	// and not stopping" is the signal. With one, the probe's pass/
	// fail overrides — a container that's Running but failing its
	// HTTP /healthz is correctly reported as NotReady, and
	// cluster-svc's PodReady != False filter routes around it.
	readyState := corev1.ConditionFalse
	readyReason := ""
	libpodReady := ins.State.Running && !ins.State.Restarting && !ins.State.Paused
	switch {
	case libpodReady:
		readyState = corev1.ConditionTrue
	case ins.State.Status == "exited" || ins.State.Status == "stopped" || ins.State.Dead:
		readyReason = "ContainersNotReady"
	default:
		readyReason = "ContainersNotReady"
	}
	if libpodReady && len(pod.Spec.Containers) > 0 {
		// First container's readinessProbe applies — vkpodman is a
		// one-container-per-pod provider today (BuildSpec rejects
		// multi-container specs); when that changes the probe loop
		// here should iterate all containers and AND their results.
		c := pod.Spec.Containers[0]
		fallbackPort := firstContainerHostPortFromSpec(&c)
		if err := runReadinessProbe(ctx, c, fallbackPort, ins.State.StartedAt); err != nil {
			readyState = corev1.ConditionFalse
			readyReason = "ReadinessProbeFailed"
			// cs.Ready mirrors the pod-level Ready so kubectl get pods
			// and the apiserver's endpoint controller see the same
			// truth.
			cs.Ready = false
		}
	}
	now := metav1.Now()
	status.Conditions = append(status.Conditions,
		corev1.PodCondition{
			Type:               corev1.PodReady,
			Status:             readyState,
			LastTransitionTime: now,
			Reason:             readyReason,
		},
		corev1.PodCondition{
			Type:               corev1.ContainersReady,
			Status:             readyState,
			LastTransitionTime: now,
			Reason:             readyReason,
		},
	)
	return status
}

func phaseFromContainer(cs corev1.ContainerStatus) corev1.PodPhase {
	switch {
	case cs.State.Running != nil:
		return corev1.PodRunning
	case cs.State.Terminated != nil:
		if cs.State.Terminated.ExitCode == 0 {
			return corev1.PodSucceeded
		}
		return corev1.PodFailed
	default:
		return corev1.PodPending
	}
}

func terminatedReason(s InspectState) string {
	switch {
	case s.OOMKilled:
		return "OOMKilled"
	case s.ExitCode == 0:
		return "Completed"
	default:
		return "Error"
	}
}

func waitingReason(s InspectState) string {
	switch s.Status {
	case "created", "":
		return "ContainerCreating"
	case "paused":
		return "Paused"
	case "stopping":
		return "ContainerStopping"
	default:
		return s.Status
	}
}

// staticHeartbeat is the interval at which NodeProvider's NotifyNodeStatus
// pushes a fresh "still alive" Node update to the apiserver. Virtual-
// kubelet has its own lease-renewal loop that uses Ping() for fast
// heartbeats; this is the slow path for pushing status changes (capacity,
// conditions). 30s is well under the typical node-monitor-grace-period
// of 40s and the default Lease.RenewDeadline of 10s.
const staticHeartbeat = 30 * time.Second
