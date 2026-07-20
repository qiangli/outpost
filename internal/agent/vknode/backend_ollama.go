package vknode

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// nativeProcessBackend realizes Pods as native host processes instead
// of libpod containers — the other half of the Backend seam (see
// backend.go). A Pod becomes a detached exec of whatever its container
// Command/Args/Env describe: ollama serve, llama-server, rpc-server, or
// an arbitrary command in tests.
//
// There is no daemon to ask "what am I running"; the backend persists
// its own JSON registry (keyed by pod UID) under DataDir so a vknode
// restart can re-adopt the processes it launched in a prior lifetime —
// the native-process analogue of the podman backend reading back its
// outpost.io/* container labels.
//
// All process-control primitives (launch detached, is-it-alive,
// terminate-the-tree) are pulled out behind struct-field hooks so the
// OS-specific pieces live in build-tagged siblings
// (backend_ollama_unix.go / backend_ollama_windows.go; legacy names)
// and tests can
// swap in a throwaway helper process.
type nativeProcessBackend struct {
	dataDir string // registry + per-process log dir
	hostIP  string // readiness-dial / base-URL host (default 127.0.0.1)
	image   string // marker image recorded when a Pod omits one

	// Hooks — defaulted by NewNativeProcessBackend, overridable in tests.
	lookPath  func(name string) (string, error)
	launch    func(ctx context.Context, spec launchSpec) (int, error)
	alive     func(pid int) bool
	terminate func(pid int) error
	ready     func(ctx context.Context, e procEntry) bool

	mu sync.Mutex // serializes registry read-modify-write
}

// NativeProcessConfig configures a native-process Backend. Only DataDir is
// required; the rest fall back to sane defaults.
type NativeProcessConfig struct {
	DataDir string // where the JSON registry + process logs live
	HostIP  string // host the process is reachable on (default 127.0.0.1)
	Image   string // image recorded when a Pod omits one
}

// OllamaConfig is the legacy config alias for NewOllamaBackend.
type OllamaConfig = NativeProcessConfig

// DefaultNativeProcessImage is the placeholder image recorded when a
// native-process Pod omits spec.containers[].image.
const DefaultNativeProcessImage = "dhnt.io/native-process"

// DefaultOllamaImage is retained for manifests/tests that use the
// original ollama marker image.
const DefaultOllamaImage = "dhnt.io/ollama"

// NewNativeProcessBackend returns a Backend that realizes Pods as native host
// processes, persisting its process registry under cfg.DataDir.
func NewNativeProcessBackend(cfg NativeProcessConfig) (Backend, error) {
	if cfg.DataDir == "" {
		return nil, fmt.Errorf("vknode: native-process backend requires a DataDir")
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("vknode: create native-process data dir: %w", err)
	}
	b := &nativeProcessBackend{
		dataDir:   cfg.DataDir,
		hostIP:    cfg.HostIP,
		image:     cfg.Image,
		lookPath:  defaultLookPath,
		launch:    defaultLaunch,
		alive:     processAlive,
		terminate: killProcessTree,
	}
	if b.hostIP == "" {
		b.hostIP = "127.0.0.1"
	}
	if b.image == "" {
		b.image = DefaultNativeProcessImage
	}
	b.ready = b.tcpReady
	return b, nil
}

// NewOllamaBackend returns a native-process Backend using the legacy
// ollama marker image by default.
func NewOllamaBackend(cfg OllamaConfig) (Backend, error) {
	if cfg.Image == "" {
		cfg.Image = DefaultOllamaImage
	}
	return NewNativeProcessBackend(cfg)
}

// ollamaBackend is a compatibility alias for older tests/callers that
// reached into the unexported concrete type inside package vknode.
type ollamaBackend = nativeProcessBackend

// procEntry is one persisted registry row: everything needed to adopt,
// probe, hydrate ports for, and terminate a process across a vknode
// restart. Serialized to <dataDir>/registry.json keyed by pod UID.
type procEntry struct {
	Namespace  string            `json:"namespace"`
	Name       string            `json:"name"`
	Container  string            `json:"container"`
	Image      string            `json:"image"`
	Command    []string          `json:"command,omitempty"`
	Args       []string          `json:"args,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	Ports      []procPort        `json:"ports,omitempty"`
	PID        int               `json:"pid"`
	StartedAt  time.Time         `json:"started_at"`
	Exited     bool              `json:"exited,omitempty"`
	ExitCode   int32             `json:"exit_code,omitempty"`
	FinishedAt time.Time         `json:"finished_at,omitempty"`
}

// procPort records one resolved containerPort→hostPort mapping. For a
// native process there is no NAT, so the process is expected to LISTEN
// on HostPort directly (that's what readiness dials and what cloudbox
// reaches).
type procPort struct {
	ContainerPort int32  `json:"container_port"`
	HostPort      int32  `json:"host_port"`
	Protocol      string `json:"protocol,omitempty"`
}

// launchSpec is the OS-agnostic description handed to the launch hook.
type launchSpec struct {
	Path    string   // resolved absolute binary path
	Args    []string // args after argv[0]
	Env     []string // full environment ("K=V" entries)
	Dir     string   // working directory ("" = inherit)
	LogPath string   // file to redirect stdout+stderr into
	OnExit  func(pid int, exitCode int32, finishedAt time.Time)
}

// Ensure starts (or adopts) the native process backing pod. Idempotent
// by pod UID: if the registry already has a live process for this UID
// we adopt it — hydrate the resolved ports back onto the in-memory pod
// and return without launching a second copy. Otherwise we exec the
// Pod's Command/Args/Env as a detached process and persist a fresh
// registry row.
func (b *nativeProcessBackend) Ensure(ctx context.Context, pod *corev1.Pod) error {
	if pod == nil {
		return fmt.Errorf("vknode: nil Pod")
	}
	if n := len(pod.Spec.Containers); n != 1 {
		return fmt.Errorf("vknode: native-process backend supports exactly one container per Pod (got %d)", n)
	}
	uid := string(pod.UID)
	if uid == "" {
		return fmt.Errorf("vknode: pod %s has empty UID", podKey(pod.Namespace, pod.Name))
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	reg, err := b.loadRegistry()
	if err != nil {
		return err
	}

	// Adopt a survivor from a prior incarnation: a registry row whose
	// process is still alive. Hydrate the recorded ports onto the
	// in-memory pod so the Provider's publishPod sees the same hostPort
	// the original launch chose, then no-op.
	if e, ok := reg[uid]; ok && b.alive(e.PID) {
		hydratePodPortsFromEntry(pod, e)
		slog.Info("vknode: adopted existing process",
			"pod", podKey(pod.Namespace, pod.Name), "pid", e.PID)
		return nil
	}
	if e, ok := reg[uid]; ok && e.Exited {
		hydratePodPortsFromEntry(pod, e)
		return nil
	}

	c := pod.Spec.Containers[0]
	argv := append(append([]string{}, c.Command...), c.Args...)
	if len(argv) == 0 {
		return fmt.Errorf("vknode: pod %s has empty command (native-process backend needs an exec target)",
			podKey(pod.Namespace, pod.Name))
	}
	bin, err := b.lookPath(argv[0])
	if err != nil {
		return fmt.Errorf("vknode: resolve binary %q for pod %s: %w",
			argv[0], podKey(pod.Namespace, pod.Name), err)
	}

	image := c.Image
	if image == "" {
		image = b.image
	}
	entry := procEntry{
		Namespace: pod.Namespace,
		Name:      pod.Name,
		Container: ContainerName(pod),
		Image:     image,
		Command:   append([]string(nil), c.Command...),
		Args:      append([]string(nil), c.Args...),
		Env:       envMap(c.Env),
		Ports:     portsFromContainer(c),
		StartedAt: time.Now(),
	}

	spec := launchSpec{
		Path:    bin,
		Args:    argv[1:],
		Env:     mergeEnv(os.Environ(), c.Env),
		Dir:     c.WorkingDir,
		LogPath: filepath.Join(b.dataDir, uid+".log"),
		OnExit: func(pid int, exitCode int32, finishedAt time.Time) {
			b.recordExit(uid, pid, exitCode, finishedAt)
		},
	}
	pid, err := b.launch(ctx, spec)
	if err != nil {
		return fmt.Errorf("vknode: launch process for pod %s: %w",
			podKey(pod.Namespace, pod.Name), err)
	}
	entry.PID = pid

	reg[uid] = entry
	if err := b.saveRegistry(reg); err != nil {
		// We started a process but couldn't record it — kill it rather
		// than leak an unowned process the registry can never reap.
		_ = b.terminate(pid)
		return fmt.Errorf("vknode: persist registry for pod %s: %w",
			podKey(pod.Namespace, pod.Name), err)
	}
	slog.Info("vknode: launched process",
		"pod", podKey(pod.Namespace, pod.Name), "pid", pid, "bin", bin)
	return nil
}

// Delete terminates the pod's process (whole group, best-effort) and
// drops its registry row. A missing/already-gone process is not an
// error — the row is removed regardless so the slot frees up.
func (b *nativeProcessBackend) Delete(ctx context.Context, pod *corev1.Pod) error {
	if pod == nil {
		return nil
	}
	uid := string(pod.UID)

	b.mu.Lock()
	defer b.mu.Unlock()

	reg, err := b.loadRegistry()
	if err != nil {
		return err
	}
	e, ok := reg[uid]
	if !ok {
		return nil // already gone
	}
	if b.alive(e.PID) {
		if terr := b.terminate(e.PID); terr != nil {
			slog.Warn("vknode: terminate process",
				"pod", podKey(pod.Namespace, pod.Name), "pid", e.PID, "err", terr)
		}
	}
	delete(reg, uid)
	if err := b.saveRegistry(reg); err != nil {
		return fmt.Errorf("vknode: persist registry after delete of pod %s: %w",
			podKey(pod.Namespace, pod.Name), err)
	}
	slog.Info("vknode: deleted process",
		"pod", podKey(pod.Namespace, pod.Name), "pid", e.PID)
	return nil
}

// Status reports the live PodStatus for pod's process. A terminal
// process returns Succeeded/Failed with the recorded exit code. A vanished
// process (no registry row, or the PID no longer alive before exit was captured) returns
// (nil, nil) so the Provider surfaces Pending/ContainerMissing and the
// reconciler recreates it. A live-but-not-yet-ready process is Pending
// with reason ContainerCreating; a live + readiness-passing process is
// Running.
func (b *nativeProcessBackend) Status(ctx context.Context, pod *corev1.Pod) (*corev1.PodStatus, error) {
	if pod == nil {
		return nil, nil
	}
	b.mu.Lock()
	reg, err := b.loadRegistry()
	if err != nil {
		b.mu.Unlock()
		return nil, err
	}
	e, ok := reg[string(pod.UID)]
	b.mu.Unlock()
	if !ok {
		return nil, nil
	}

	cname := e.Container
	if len(pod.Spec.Containers) > 0 && pod.Spec.Containers[0].Name != "" {
		cname = pod.Spec.Containers[0].Name
	}
	if e.Exited {
		return b.terminatedStatus(cname, e), nil
	}
	if !b.alive(e.PID) {
		return nil, nil
	}

	if b.ready(ctx, e) {
		return b.runningStatus(cname, e), nil
	}
	return pendingStatus(cname, e), nil
}

// List reconstructs skeleton Pods from the persisted registry. Called
// once at startup so a vknode restart re-discovers the processes it
// launched before. The reconstruction is intentionally minimal (the
// apiserver remains the source of truth for the full spec) but carries
// the identity + resolved ports so the Provider's transient-app
// republish has what it needs.
func (b *nativeProcessBackend) List(ctx context.Context) ([]*corev1.Pod, error) {
	b.mu.Lock()
	reg, err := b.loadRegistry()
	b.mu.Unlock()
	if err != nil {
		return nil, err
	}
	out := make([]*corev1.Pod, 0, len(reg))
	for uid, e := range reg {
		if e.Namespace == "" || e.Name == "" {
			slog.Warn("vknode: registry row missing identity", "uid", uid, "entry", e)
			continue
		}
		cname := e.Container
		out = append(out, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: e.Namespace,
				Name:      e.Name,
				UID:       types.UID(uid),
			},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:  cname,
					Image: e.Image,
					Ports: containerPortsFromEntry(e),
				}},
			},
		})
	}
	// Deterministic order so callers/tests don't see map iteration churn.
	sort.Slice(out, func(i, j int) bool {
		return podKey(out[i].Namespace, out[i].Name) < podKey(out[j].Namespace, out[j].Name)
	})
	return out, nil
}

// HydratePorts merges the registry's resolved host ports back onto
// pod.Spec in place. Best-effort: a missing registry row is a no-op,
// never an error.
func (b *nativeProcessBackend) HydratePorts(ctx context.Context, pod *corev1.Pod) error {
	if pod == nil {
		return nil
	}
	b.mu.Lock()
	reg, err := b.loadRegistry()
	b.mu.Unlock()
	if err != nil {
		return nil // best-effort
	}
	if e, ok := reg[string(pod.UID)]; ok {
		hydratePodPortsFromEntry(pod, e)
	}
	return nil
}

// --- registry persistence ---------------------------------------------

func (b *nativeProcessBackend) registryPath() string {
	return filepath.Join(b.dataDir, "registry.json")
}

// loadRegistry reads the on-disk registry. A missing file is an empty
// registry, not an error. Caller holds b.mu (or is read-only at
// startup).
func (b *nativeProcessBackend) loadRegistry() (map[string]procEntry, error) {
	data, err := os.ReadFile(b.registryPath())
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]procEntry{}, nil
		}
		return nil, fmt.Errorf("vknode: read process registry: %w", err)
	}
	reg := map[string]procEntry{}
	if len(data) == 0 {
		return reg, nil
	}
	if err := json.Unmarshal(data, &reg); err != nil {
		return nil, fmt.Errorf("vknode: parse process registry: %w", err)
	}
	return reg, nil
}

// saveRegistry writes the registry atomically (temp file + rename) so a
// crash mid-write can't corrupt the file. Caller holds b.mu.
func (b *nativeProcessBackend) saveRegistry(reg map[string]procEntry) error {
	data, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return err
	}
	tmp := b.registryPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, b.registryPath())
}

func (b *nativeProcessBackend) recordExit(uid string, pid int, exitCode int32, finishedAt time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()

	reg, err := b.loadRegistry()
	if err != nil {
		slog.Warn("vknode: load process registry after exit", "uid", uid, "err", err)
		return
	}
	e, ok := reg[uid]
	if !ok || e.Exited || e.PID != pid {
		return
	}
	e.Exited = true
	e.ExitCode = exitCode
	e.FinishedAt = finishedAt
	reg[uid] = e
	if err := b.saveRegistry(reg); err != nil {
		slog.Warn("vknode: persist process exit", "uid", uid, "pid", e.PID, "exitCode", exitCode, "err", err)
	}
}

// --- readiness ---------------------------------------------------------

// tcpReady is the default readiness probe: a process with no published
// ports is ready the moment it's alive; otherwise its first hostPort
// must accept a TCP connection (or answer an HTTP GET /). This mirrors
// the "port listening or HTTP GET /" contract — a TCP connect succeeds
// for both a raw listener and an HTTP server, so it's the cheaper check.
func (b *nativeProcessBackend) tcpReady(ctx context.Context, e procEntry) bool {
	if len(e.Ports) == 0 {
		return true
	}
	hp := e.Ports[0].HostPort
	if hp == 0 {
		return true
	}
	addr := net.JoinHostPort(b.hostIP, strconv.Itoa(int(hp)))
	d := net.Dialer{Timeout: time.Second}
	if ctx == nil {
		ctx = context.Background()
	}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// --- status builders ---------------------------------------------------

func (b *nativeProcessBackend) runningStatus(cname string, e procEntry) *corev1.PodStatus {
	started := metav1.NewTime(e.StartedAt)
	cs := corev1.ContainerStatus{
		Name:        cname,
		Image:       e.Image,
		ContainerID: fmt.Sprintf("process://%d", e.PID),
		Ready:       true,
		State: corev1.ContainerState{
			Running: &corev1.ContainerStateRunning{StartedAt: started},
		},
	}
	now := metav1.Now()
	return &corev1.PodStatus{
		Phase:             corev1.PodRunning,
		HostIP:            b.hostIP,
		StartTime:         &started,
		ContainerStatuses: []corev1.ContainerStatus{cs},
		Conditions: []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: now},
			{Type: corev1.ContainersReady, Status: corev1.ConditionTrue, LastTransitionTime: now},
		},
	}
}

func (b *nativeProcessBackend) terminatedStatus(cname string, e procEntry) *corev1.PodStatus {
	started := metav1.NewTime(e.StartedAt)
	finished := metav1.NewTime(e.FinishedAt)
	reason := "Error"
	phase := corev1.PodFailed
	if e.ExitCode == 0 {
		reason = "Completed"
		phase = corev1.PodSucceeded
	}
	cs := corev1.ContainerStatus{
		Name:        cname,
		Image:       e.Image,
		ContainerID: fmt.Sprintf("process://%d", e.PID),
		Ready:       false,
		State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{
				ExitCode:    e.ExitCode,
				Reason:      reason,
				ContainerID: fmt.Sprintf("process://%d", e.PID),
				StartedAt:   started,
				FinishedAt:  finished,
			},
		},
	}
	now := metav1.Now()
	return &corev1.PodStatus{
		Phase:             phase,
		HostIP:            b.hostIP,
		StartTime:         &started,
		ContainerStatuses: []corev1.ContainerStatus{cs},
		Conditions: []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionFalse, Reason: "ContainersNotReady", LastTransitionTime: now},
			{Type: corev1.ContainersReady, Status: corev1.ConditionFalse, Reason: "ContainersNotReady", LastTransitionTime: now},
		},
	}
}

func pendingStatus(cname string, e procEntry) *corev1.PodStatus {
	cs := corev1.ContainerStatus{
		Name:        cname,
		Image:       e.Image,
		ContainerID: fmt.Sprintf("process://%d", e.PID),
		Ready:       false,
		State: corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"},
		},
	}
	now := metav1.Now()
	return &corev1.PodStatus{
		Phase:             corev1.PodPending,
		ContainerStatuses: []corev1.ContainerStatus{cs},
		Conditions: []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionFalse, Reason: "ContainersNotReady", LastTransitionTime: now},
			{Type: corev1.ContainersReady, Status: corev1.ConditionFalse, Reason: "ContainersNotReady", LastTransitionTime: now},
		},
	}
}

// --- helpers -----------------------------------------------------------

// defaultLookPath resolves a binary name to an absolute path. An
// already-absolute path is returned as-is (the test helper-process
// pattern hands us os.Args[0]); a bare name goes through $PATH.
func defaultLookPath(name string) (string, error) {
	if filepath.IsAbs(name) {
		return name, nil
	}
	return exec.LookPath(name)
}

func exitCodeFromWait(err error, state *os.ProcessState) int32 {
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return int32(ee.ExitCode())
	}
	if state != nil {
		return int32(state.ExitCode())
	}
	return 1
}

// envMap flattens a container's env (Value-only — ValueFrom is rejected
// upstream at translate time) into a plain map for registry recording.
func envMap(envs []corev1.EnvVar) map[string]string {
	if len(envs) == 0 {
		return nil
	}
	out := make(map[string]string, len(envs))
	for _, e := range envs {
		out[e.Name] = e.Value
	}
	return out
}

// mergeEnv overlays a container's env onto a base ("K=V") environment,
// overriding duplicates. Used to give the launched process the host env
// (PATH/HOME/…) plus the Pod's declared overrides.
func mergeEnv(base []string, envs []corev1.EnvVar) []string {
	if len(envs) == 0 {
		return base
	}
	idx := make(map[string]int, len(base))
	out := append([]string(nil), base...)
	for i, kv := range out {
		if eq := indexByte(kv, '='); eq >= 0 {
			idx[kv[:eq]] = i
		}
	}
	for _, e := range envs {
		kv := e.Name + "=" + e.Value
		if i, ok := idx[e.Name]; ok {
			out[i] = kv
		} else {
			idx[e.Name] = len(out)
			out = append(out, kv)
		}
	}
	return out
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// portsFromContainer records a container's declared ports (with their
// resolved HostPort) for the registry. The Provider has already run
// AllocateMissingHostPorts before Ensure, so HostPort is set.
func portsFromContainer(c corev1.Container) []procPort {
	if len(c.Ports) == 0 {
		return nil
	}
	out := make([]procPort, 0, len(c.Ports))
	for _, p := range c.Ports {
		proto := string(p.Protocol)
		if proto == "" {
			proto = string(corev1.ProtocolTCP)
		}
		out = append(out, procPort{
			ContainerPort: p.ContainerPort,
			HostPort:      p.HostPort,
			Protocol:      proto,
		})
	}
	return out
}

// containerPortsFromEntry rebuilds []ContainerPort for a List skeleton.
func containerPortsFromEntry(e procEntry) []corev1.ContainerPort {
	if len(e.Ports) == 0 {
		return nil
	}
	out := make([]corev1.ContainerPort, 0, len(e.Ports))
	for _, p := range e.Ports {
		proto := corev1.Protocol(p.Protocol)
		if proto == "" {
			proto = corev1.ProtocolTCP
		}
		out = append(out, corev1.ContainerPort{
			ContainerPort: p.ContainerPort,
			HostPort:      p.HostPort,
			Protocol:      proto,
		})
	}
	return out
}

// hydratePodPortsFromEntry fills pod.Spec.Containers[].Ports[].HostPort
// from the registry entry, matching on containerPort. Explicit
// (non-zero) HostPorts in the live pod are left alone — the registry is
// only authoritative for ports vknode allocated. Mirrors
// HydratePodPortsFromLabels for the native-process backend.
func hydratePodPortsFromEntry(pod *corev1.Pod, e procEntry) {
	if pod == nil || len(e.Ports) == 0 {
		return
	}
	byCP := make(map[int32]int32, len(e.Ports))
	for _, p := range e.Ports {
		byCP[p.ContainerPort] = p.HostPort
	}
	for ci := range pod.Spec.Containers {
		c := &pod.Spec.Containers[ci]
		for pi := range c.Ports {
			p := &c.Ports[pi]
			if p.HostPort != 0 {
				continue
			}
			if hp, ok := byCP[p.ContainerPort]; ok && hp != 0 {
				p.HostPort = hp
			}
		}
	}
}
