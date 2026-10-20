package vknode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qiangli/outpost/internal/agent/ollama"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Worker bootstrap (Sprint 451 S451.9).
//
// A CPU/GPU worker is an ordinary Outpost-paired host that additionally
// registers ONE virtual-kubelet node with the satellite's DKS plane and
// ONE peer with the sphere/model pool, with ONE native-process engine
// (the vk-ollama pod on BashyManagedPort) as the only model-serving owner.
// Bootstrap binds all three to one physical-host identity — machine_id,
// with the hostname fallback of S450.16 decision 4 — and reconciles by
// that identity, so running it again (retry, vknode restart, reboot)
// yields the same node name, the same sphere peer ID, the same scoped
// credential set and the same engine registry row rather than a second
// of any of them.
//
// Nothing here is a new satellite identity, role, pairing protocol or
// host daemon: the DKS credential is the existing scoped enrollment
// credential (cloudbox's per-host kubeconfig mint, or a peer plane's
// scoped node credential — never a cluster-admin kubeconfig or provider
// credential), the peer ID is the existing mesh key, and the engine is
// the existing native-process backend's pod-UID registry.
//
// Phases are tracked independently and persisted under StateDir. The
// bootstrap owns dks_joined and sphere_joined; host_created and
// bashy_installed belong to the provisioner/installer (MarkPhase), and
// model_ready belongs to S451.5. A partial failure persists what did
// succeed, returns the error, and is resumed by the next call; it never
// reports WorkerReady.

// Phase is one independently tracked bootstrap milestone.
type Phase string

const (
	PhaseHostCreated    Phase = "host_created"
	PhaseBashyInstalled Phase = "bashy_installed"
	PhaseDKSJoined      Phase = "dks_joined"
	PhaseSphereJoined   Phase = "sphere_joined"
	PhaseModelReady     Phase = "model_ready"
)

// DefaultBootstrapBackend is the virtual-kubelet backend a worker
// registers when none is named: the native-process backend that owns the
// model-serving pod (S451.1 decision (a)).
const DefaultBootstrapBackend = "vk-ollama"

// enrollStateFile is the persisted bootstrap state under StateDir.
const enrollStateFile = "enroll.json"

// enrollStateVersion guards the on-disk shape.
const enrollStateVersion = 1

// EnrollState is the durable record of one worker's enrollment. It is
// what a restart or reboot reads back to reconcile instead of re-enroll.
type EnrollState struct {
	Version int `json:"version"`

	// MachineID is the physical-host identity every other field is bound
	// to. A state file whose MachineID differs from the running host's is
	// refused (a cloned disk must not impersonate the original worker).
	MachineID string `json:"machine_id"`
	// NodeName is the DKS node registered for this host — fixed on first
	// bootstrap and reused forever after, even if the hostname changes.
	NodeName string `json:"node_name"`
	// Backend is the virtual-kubelet backend the node runs (vk-ollama).
	Backend string `json:"backend"`
	// PairingID identifies the satellite pairing the DKS join was made
	// against. A different value on the next bootstrap invalidates the
	// join: re-pairing must not ride a cached cluster credential/API
	// (Sprint 482 45a78924).
	PairingID string `json:"pairing_id,omitempty"`

	// PeerID is the sphere peer identity (libp2p peer ID of the mesh key).
	PeerID string `json:"peer_id,omitempty"`

	// APIURL and CredentialFingerprint describe the scoped DKS credential
	// set in use. The token itself lives only in TokenFile (0600).
	APIURL                string `json:"api_url,omitempty"`
	CredentialFingerprint string `json:"credential_fingerprint,omitempty"`
	TokenFile             string `json:"token_file,omitempty"`
	CAFile                string `json:"ca_file,omitempty"`

	// EngineUID is the stable pod UID of the native-process engine row.
	EngineUID string `json:"engine_uid,omitempty"`
	// EngineURL is the engine's base URL; the sphere watcher's OllamaURL
	// points here.
	EngineURL string `json:"engine_url,omitempty"`

	// Phases maps each completed phase to its completion time.
	Phases map[Phase]time.Time `json:"phases,omitempty"`
	// LastError is the most recent bootstrap failure, cleared on success.
	LastError string    `json:"last_error,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Done reports whether phase has completed.
func (s *EnrollState) Done(p Phase) bool {
	if s == nil || s.Phases == nil {
		return false
	}
	_, ok := s.Phases[p]
	return ok
}

func (s *EnrollState) mark(p Phase, now time.Time) {
	if s.Phases == nil {
		s.Phases = map[Phase]time.Time{}
	}
	s.Phases[p] = now
}

func (s *EnrollState) clear(p Phase) {
	if s.Phases != nil {
		delete(s.Phases, p)
	}
}

// WorkerReady reports whether the worker is enrolled on BOTH sides —
// DKS joined and sphere joined. The two are tracked separately on
// purpose: a worker with one link down withdraws that side's capacity,
// and partial readiness is never advertised as full. Model readiness
// (S451.5) is a further, separate phase on top of this.
func (s *EnrollState) WorkerReady() bool {
	return s.Done(PhaseDKSJoined) && s.Done(PhaseSphereJoined)
}

// ModelWorkerReady is the full model-worker readiness: WorkerReady plus
// the S451.5 model_ready phase. Bootstrap never sets model_ready itself.
func (s *EnrollState) ModelWorkerReady() bool {
	return s.WorkerReady() && s.Done(PhaseModelReady)
}

// EnrollStatePath is the file LoadEnrollState / SaveEnrollState use.
func EnrollStatePath(stateDir string) string {
	return filepath.Join(stateDir, enrollStateFile)
}

// LoadEnrollState reads the persisted state. A missing file is a fresh
// (nil-free, zero) state, not an error.
func LoadEnrollState(stateDir string) (*EnrollState, error) {
	if stateDir == "" {
		return nil, errors.New("vknode: enroll: empty state dir")
	}
	data, err := os.ReadFile(EnrollStatePath(stateDir))
	if err != nil {
		if os.IsNotExist(err) {
			return &EnrollState{Version: enrollStateVersion}, nil
		}
		return nil, fmt.Errorf("vknode: enroll: read state: %w", err)
	}
	var st EnrollState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("vknode: enroll: parse state: %w", err)
	}
	if st.Version == 0 {
		st.Version = enrollStateVersion
	}
	return &st, nil
}

// SaveEnrollState persists st atomically (0600) under stateDir.
func SaveEnrollState(stateDir string, st *EnrollState) error {
	if stateDir == "" {
		return errors.New("vknode: enroll: empty state dir")
	}
	if st == nil {
		return errors.New("vknode: enroll: nil state")
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("vknode: enroll: state dir: %w", err)
	}
	st.Version = enrollStateVersion
	st.UpdatedAt = time.Now().UTC()
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("vknode: enroll: encode state: %w", err)
	}
	return writeFileAtomic(EnrollStatePath(stateDir), data)
}

// MarkPhase records a phase the bootstrap does not own (host_created,
// bashy_installed from the provisioner/installer; model_ready from the
// S451.5 model step) without touching anything else.
func MarkPhase(stateDir string, p Phase) error {
	st, err := LoadEnrollState(stateDir)
	if err != nil {
		return err
	}
	st.mark(p, time.Now().UTC())
	return SaveEnrollState(stateDir, st)
}

// ScopedCredential is the scoped DKS enrollment credential a DKSJoiner
// returns: exactly the per-host ServiceAccount token cloudbox mints
// (FetchKubeconfig) or a peer plane's scoped node credential. It is
// never a cluster-admin kubeconfig or a provider credential.
type ScopedCredential struct {
	APIURL string
	Token  string
	CA     []byte
}

// DKSJoiner mints the scoped credential that registers nodeName with the
// satellite's DKS plane. CloudboxDKSJoiner is the production form.
type DKSJoiner interface {
	Join(ctx context.Context, nodeName string) (*ScopedCredential, error)
}

// CloudboxDKSJoiner joins through the existing Outpost pairing: the
// paired access_token asks cloudbox's per-host kubeconfig endpoint for
// the node's scoped ServiceAccount token (FetchKubeconfig).
type CloudboxDKSJoiner struct {
	CloudboxBase string
	AccessToken  string
}

// Join implements DKSJoiner.
func (j CloudboxDKSJoiner) Join(ctx context.Context, nodeName string) (*ScopedCredential, error) {
	kc, err := FetchKubeconfig(ctx, j.CloudboxBase, j.AccessToken, nodeName)
	if err != nil {
		return nil, err
	}
	return &ScopedCredential{APIURL: kc.APIURL, Token: kc.Token, CA: kc.CA}, nil
}

// SphereJoinRequest is what the sphere/model-pool side is asked to enroll.
type SphereJoinRequest struct {
	MachineID string
	NodeName  string
	PeerID    string
	// EngineURL is the native-process engine's base URL — the one engine
	// the sphere watcher's OllamaURL must point at.
	EngineURL string
}

// SphereJoiner resolves the host's stable sphere peer identity and enrolls
// it in the model pool. Identity is keyed by stateDir so a restart reads
// the same ID back (mesh.PeerIDForDir is the production form).
type SphereJoiner interface {
	Identity(stateDir string) (string, error)
	Join(ctx context.Context, req SphereJoinRequest) error
}

// EngineSpec describes the single native-process engine the worker owns.
// It is realized as one pod on the native-process backend with a UID
// derived from the node name, so Ensure adopts the running process on
// restart instead of launching a second engine.
type EngineSpec struct {
	// Command/Args/Env are the engine's exec target (e.g. ollama serve).
	Command []string
	Args    []string
	Env     map[string]string
	// Port is the engine's listen port; the pod's hostPort and the
	// EngineURL both use it. Zero means ollama.BashyManagedPort (11435).
	Port int32
	// Namespace/Name are the engine pod's identity in the registry;
	// defaults are outpost-system / ollama-engine.
	Namespace string
	Name      string
	// Image is the marker image recorded for the pod (default: the
	// backend's own marker).
	Image string
}

// defaultEnginePort uses the service's declaring constant so bootstrap and
// the managed engine cannot drift to different ports.
const defaultEnginePort int32 = ollama.BashyManagedPort

// BootstrapOptions configures one Bootstrap run.
type BootstrapOptions struct {
	// MachineID is the stable physical-host identity (machineid.Get).
	// Hostname is the fallback when it is unavailable.
	MachineID string
	Hostname  string
	// NodeName optionally overrides the derived node base name.
	NodeName string
	// Backend is the virtual-kubelet backend (default vk-ollama).
	Backend string
	// PairingID identifies the satellite pairing in force (for example
	// the cloudbox base URL plus agent name). A change invalidates a
	// previous DKS join.
	PairingID string

	// StateDir holds enroll.json, the scoped credential files and the
	// mesh identity used for the sphere peer ID.
	StateDir string

	// Engine is the native-process backend; EngineSpec what to run on it.
	// Both nil/zero skips the engine step (DKS/sphere-only enrollment).
	Engine     Backend
	EngineSpec EngineSpec

	DKS    DKSJoiner
	Sphere SphereJoiner
}

// BootstrapResult reports the reconciled identities after a run.
type BootstrapResult struct {
	State                 *EnrollState
	NodeName              string
	PeerID                string
	CredentialFingerprint string
	TokenFile             string
	CAFile                string
	EngineUID             string
	EngineURL             string
	// EnginePod is the pod handed to the backend (nil when no engine).
	EnginePod *corev1.Pod
}

// DeriveNodeName returns the DKS node name for a host: the operator
// override, else "host-<12 hex of machine_id>", else the sanitized
// hostname's first label — suffixed with the backend the way the daemon
// names its virtual nodes (<base>-<backend>).
func DeriveNodeName(override, machineID, hostname, backend string) (string, error) {
	if backend == "" {
		backend = DefaultBootstrapBackend
	}
	base := strings.TrimSpace(override)
	switch {
	case base != "":
	case strings.TrimSpace(machineID) != "":
		id := strings.ToLower(strings.TrimSpace(machineID))
		if len(id) > 12 {
			id = id[:12]
		}
		base = "host-" + id
	case strings.TrimSpace(hostname) != "":
		h := strings.ToLower(strings.TrimSpace(hostname))
		if i := strings.IndexByte(h, '.'); i > 0 {
			h = h[:i]
		}
		base = h
	default:
		return "", errors.New("vknode: enroll: no node identity (machine_id, hostname or override)")
	}
	name := strings.ToLower(sanitizeLabelValue(base + "-" + backend))
	name = strings.ReplaceAll(name, "_", "-")
	name = strings.Trim(name, "-.")
	if name == "" {
		return "", fmt.Errorf("vknode: enroll: node name derived from %q is empty", base)
	}
	return name, nil
}

// Bootstrap enrolls (or re-reconciles) this host as a worker. It is
// idempotent by identity: see the package comment above.
//
// On error the partial result is returned alongside it (State reflects
// exactly the phases that did complete, and LastError is persisted), so
// the caller can report progress and the next call resumes from there.
func Bootstrap(ctx context.Context, opts BootstrapOptions) (*BootstrapResult, error) {
	if opts.StateDir == "" {
		return nil, errors.New("vknode: enroll: StateDir required")
	}
	if opts.DKS == nil {
		return nil, errors.New("vknode: enroll: DKS joiner required")
	}
	if opts.Sphere == nil {
		return nil, errors.New("vknode: enroll: sphere joiner required")
	}
	if strings.TrimSpace(opts.PairingID) == "" {
		return nil, errors.New("vknode: enroll: pairing identity required")
	}
	backend := opts.Backend
	if backend == "" {
		backend = DefaultBootstrapBackend
	}

	st, err := LoadEnrollState(opts.StateDir)
	if err != nil {
		return nil, err
	}
	machineID := strings.TrimSpace(opts.MachineID)
	if st.MachineID != "" && machineID != "" && st.MachineID != machineID {
		return nil, fmt.Errorf("vknode: enroll: state in %s belongs to machine_id %s…, not this host (%s…)",
			opts.StateDir, prefix(st.MachineID, 8), prefix(machineID, 8))
	}
	if st.Backend != "" && st.Backend != backend {
		return nil, fmt.Errorf("vknode: enroll: state in %s is for backend %q, not %q",
			opts.StateDir, st.Backend, backend)
	}
	if st.MachineID == "" {
		st.MachineID = machineID
	}
	st.Backend = backend

	// --- identity: node name + sphere peer ID -----------------------------
	if st.NodeName == "" {
		name, err := DeriveNodeName(opts.NodeName, machineID, opts.Hostname, backend)
		if err != nil {
			return nil, err
		}
		st.NodeName = name
	}
	peerID, err := opts.Sphere.Identity(opts.StateDir)
	if err != nil {
		return nil, fmt.Errorf("vknode: enroll: sphere identity: %w", err)
	}
	if st.PeerID != "" && st.PeerID != peerID {
		return nil, fmt.Errorf("vknode: enroll: sphere peer id changed (%s… -> %s…); refusing to enroll a second peer for one host",
			prefix(st.PeerID, 8), prefix(peerID, 8))
	}
	st.PeerID = peerID
	if err := SaveEnrollState(opts.StateDir, st); err != nil {
		return nil, err
	}

	res := &BootstrapResult{State: st, NodeName: st.NodeName, PeerID: st.PeerID}
	fail := func(step string, err error) (*BootstrapResult, error) {
		st.LastError = step + ": " + err.Error()
		if serr := SaveEnrollState(opts.StateDir, st); serr != nil {
			slog.Warn("vknode: enroll: persist failure state", "err", serr)
		}
		fill(res, st)
		return res, fmt.Errorf("vknode: enroll: %s: %w", step, err)
	}

	// --- phase: dks_joined --------------------------------------------------
	if st.PairingID != opts.PairingID {
		// A different satellite has a different sphere pool as well as a
		// different DKS plane. Keep the host/peer identities but re-enroll
		// both sides and recheck model readiness there.
		st.clear(PhaseSphereJoined)
		st.clear(PhaseModelReady)
	}
	if st.Done(PhaseDKSJoined) && !dksJoinValid(st, opts.PairingID) {
		slog.Info("vknode: enroll: cached DKS join invalid; re-joining",
			"node", st.NodeName, "reason", dksInvalidReason(st, opts.PairingID))
		st.clear(PhaseDKSJoined)
	}
	if !st.Done(PhaseDKSJoined) {
		cred, err := opts.DKS.Join(ctx, st.NodeName)
		if err != nil {
			return fail("dks join", err)
		}
		if cred == nil || strings.TrimSpace(cred.Token) == "" || strings.TrimSpace(cred.APIURL) == "" {
			return fail("dks join", errors.New("joiner returned an incomplete scoped credential"))
		}
		tokenFile, caFile, err := materializeScoped(opts.StateDir, cred)
		if err != nil {
			return fail("dks credential", err)
		}
		st.APIURL = cred.APIURL
		st.TokenFile = tokenFile
		st.CAFile = caFile
		st.CredentialFingerprint = fingerprint(cred.Token)
		st.PairingID = opts.PairingID
		st.mark(PhaseDKSJoined, time.Now().UTC())
		if err := SaveEnrollState(opts.StateDir, st); err != nil {
			return fail("persist dks join", err)
		}
		slog.Info("vknode: enroll: DKS joined", "node", st.NodeName, "api", st.APIURL)
	}

	// --- engine: one native-process registry row ----------------------------
	if opts.Engine != nil && len(opts.EngineSpec.Command) > 0 {
		pod, url := engineSpecPod(st.NodeName, backend, opts.EngineSpec)
		if err := opts.Engine.Ensure(ctx, pod); err != nil {
			return fail("engine", err)
		}
		st.EngineUID = string(pod.UID)
		st.EngineURL = url
		res.EnginePod = pod
		if err := SaveEnrollState(opts.StateDir, st); err != nil {
			return fail("persist engine", err)
		}
	}

	// --- phase: sphere_joined -------------------------------------------------
	if !st.Done(PhaseSphereJoined) {
		req := SphereJoinRequest{
			MachineID: st.MachineID,
			NodeName:  st.NodeName,
			PeerID:    st.PeerID,
			EngineURL: st.EngineURL,
		}
		if err := opts.Sphere.Join(ctx, req); err != nil {
			return fail("sphere join", err)
		}
		st.mark(PhaseSphereJoined, time.Now().UTC())
		slog.Info("vknode: enroll: sphere joined", "node", st.NodeName, "peer", prefix(st.PeerID, 12))
	}

	st.LastError = ""
	if err := SaveEnrollState(opts.StateDir, st); err != nil {
		return nil, err
	}
	fill(res, st)
	return res, nil
}

func fill(res *BootstrapResult, st *EnrollState) {
	res.State = st
	res.NodeName = st.NodeName
	res.PeerID = st.PeerID
	res.CredentialFingerprint = st.CredentialFingerprint
	res.TokenFile = st.TokenFile
	res.CAFile = st.CAFile
	res.EngineUID = st.EngineUID
	res.EngineURL = st.EngineURL
}

// dksJoinValid decides whether a recorded DKS join can be reused: the
// same pairing, the credential files still present with the recorded
// token, and the token not expired. Anything else re-joins.
func dksJoinValid(st *EnrollState, pairingID string) bool {
	return dksInvalidReason(st, pairingID) == ""
}

func dksInvalidReason(st *EnrollState, pairingID string) string {
	if st.PairingID != pairingID {
		return "pairing changed"
	}
	if st.TokenFile == "" || st.APIURL == "" {
		return "no credential recorded"
	}
	tok, err := os.ReadFile(st.TokenFile)
	if err != nil {
		return "token file unreadable"
	}
	if fingerprint(string(tok)) != st.CredentialFingerprint {
		return "token file does not match recorded credential"
	}
	if exp := TokenExpiry(string(tok)); !exp.IsZero() && !time.Now().Before(exp) {
		return "scoped token expired"
	}
	return ""
}

// materializeScoped writes the scoped credential to 0600 files under
// <stateDir>/dks. Same bytes → same files (atomic rename), so a restart
// re-materializes without churn.
func materializeScoped(stateDir string, cred *ScopedCredential) (tokenFile, caFile string, err error) {
	dir := filepath.Join(stateDir, "dks")
	tokenFile = filepath.Join(dir, "token")
	if err := WriteTokenFile(tokenFile, cred.Token); err != nil {
		return "", "", err
	}
	if len(cred.CA) > 0 {
		caFile = filepath.Join(dir, "ca.crt")
		if err := writeFileAtomic(caFile, cred.CA); err != nil {
			return "", "", err
		}
	}
	return tokenFile, caFile, nil
}

// engineSpecPod builds the single engine pod. The UID is a function of
// the node name so every bootstrap on this host lands on the SAME
// registry row — that is what lets the backend adopt instead of launch.
func engineSpecPod(nodeName, backend string, spec EngineSpec) (*corev1.Pod, string) {
	port := spec.Port
	if port == 0 {
		port = defaultEnginePort
	}
	ns := spec.Namespace
	if ns == "" {
		ns = "outpost-system"
	}
	name := spec.Name
	if name == "" {
		name = "ollama-engine"
	}
	uid := EngineUID(nodeName)
	env := make([]corev1.EnvVar, 0, len(spec.Env))
	for _, k := range sortedKeys(spec.Env) {
		env = append(env, corev1.EnvVar{Name: k, Value: spec.Env[k]})
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns,
			Name:      name,
			UID:       types.UID(uid),
			Labels: map[string]string{
				NodeHostLabel:               nodeName,
				"outpost.dhnt.io/backend":   backend,
				"outpost.dhnt.io/engine":    "true",
				"outpost.dhnt.io/engine-id": uid,
			},
		},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
			Containers: []corev1.Container{{
				Name:    "engine",
				Image:   spec.Image,
				Command: append([]string(nil), spec.Command...),
				Args:    append([]string(nil), spec.Args...),
				Env:     env,
				Ports:   []corev1.ContainerPort{{ContainerPort: port, HostPort: port, Protocol: corev1.ProtocolTCP}},
			}},
		},
	}
	return pod, fmt.Sprintf("http://127.0.0.1:%d", port)
}

// EngineUID is the deterministic registry key of a node's engine pod.
func EngineUID(nodeName string) string {
	return "engine-" + fingerprint(nodeName)[:16]
}

func fingerprint(s string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(s)))
	return hex.EncodeToString(sum[:])
}

func prefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// insertion sort: tiny maps, no sort import churn
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
