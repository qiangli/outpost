package vknode

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/qiangli/outpost/internal/agent/mesh"
)

// fakeDKS is the DKS enrollment double. Every Join mints a DIFFERENT
// scoped token so a bootstrap that re-joins on restart is caught by the
// credential fingerprint, not just by the call counter.
type fakeDKS struct {
	joins int
	names map[string]int
	fail  error
}

func (f *fakeDKS) Join(_ context.Context, nodeName string) (*ScopedCredential, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	f.joins++
	if f.names == nil {
		f.names = map[string]int{}
	}
	f.names[nodeName]++
	return &ScopedCredential{
		APIURL: "https://127.0.0.1:6443",
		Token:  "scoped-sa-token-" + strconv.Itoa(f.joins),
		CA:     []byte("-----BEGIN CERTIFICATE-----\nfake\n-----END CERTIFICATE-----\n"),
	}, nil
}

// fakeSphere is the sphere/model-pool double. Identity is the REAL mesh
// key derivation (the libp2p peer ID of <stateDir>/mesh_ed25519) so the
// test proves the persisted identity survives a restart; Join just
// records what it was asked to enroll.
type fakeSphere struct {
	joins int
	peers map[string]int
	fail  error
	last  SphereJoinRequest
}

func (f *fakeSphere) Identity(stateDir string) (string, error) {
	return mesh.PeerIDForDir(stateDir)
}

func (f *fakeSphere) Join(_ context.Context, req SphereJoinRequest) error {
	if f.fail != nil {
		return f.fail
	}
	f.joins++
	if f.peers == nil {
		f.peers = map[string]int{}
	}
	f.peers[req.PeerID]++
	f.last = req
	return nil
}

// helperEngineSpec builds an EngineSpec that re-execs the test binary in
// "serve" mode on a free loopback port — the native-process stand-in for
// the vk-ollama engine on BashyManagedPort.
func helperEngineSpec(t *testing.T) EngineSpec {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return EngineSpec{
		Command: []string{exe},
		Args:    []string{"-test.run=^TestHelperProcess$"},
		Env: map[string]string{
			"GO_WANT_HELPER_PROCESS": "1",
			"HELPER_MODE":            "serve",
			"HELPER_PORT":            strconv.Itoa(port),
		},
		Port: int32(port),
	}
}

// bootstrapHarness is one "incarnation" of the vknode: a freshly
// constructed native-process backend over the shared DataDir plus the
// shared doubles. A simulated restart = a new harness over the same dirs.
type bootstrapHarness struct {
	be       *nativeProcessBackend
	launches *int
}

func newBootstrapHarness(t *testing.T, dataDir string, launches *int) *bootstrapHarness {
	t.Helper()
	b, err := NewOllamaBackend(OllamaConfig{DataDir: dataDir})
	if err != nil {
		t.Fatalf("NewOllamaBackend: %v", err)
	}
	be := b.(*nativeProcessBackend)
	orig := be.launch
	be.launch = func(ctx context.Context, spec launchSpec) (int, error) {
		*launches++
		return orig(ctx, spec)
	}
	return &bootstrapHarness{be: be, launches: launches}
}

func TestBootstrap_RestartDoesNotDuplicateNodeOrPeer(t *testing.T) {
	ctx := context.Background()
	stateDir := filepath.Join(t.TempDir(), "state")
	dataDir := filepath.Join(t.TempDir(), "vk-ollama")
	const machineID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	dks := &fakeDKS{}
	sphere := &fakeSphere{}
	engine := helperEngineSpec(t)
	launches := 0

	opts := func(h *bootstrapHarness) BootstrapOptions {
		return BootstrapOptions{
			MachineID:  machineID,
			Hostname:   "worker-host.local",
			PairingID:  "pairing-1",
			StateDir:   stateDir,
			Engine:     h.be,
			EngineSpec: engine,
			DKS:        dks,
			Sphere:     sphere,
		}
	}

	// First incarnation.
	h1 := newBootstrapHarness(t, dataDir, &launches)
	r1, err := Bootstrap(ctx, opts(h1))
	if err != nil {
		t.Fatalf("Bootstrap #1: %v", err)
	}
	t.Cleanup(func() {
		if r1 != nil {
			_ = h1.be.Delete(ctx, r1.EnginePod)
		}
	})
	if !r1.State.WorkerReady() {
		t.Fatalf("after first bootstrap WorkerReady=false; state=%+v", r1.State)
	}
	if r1.NodeName == "" || r1.PeerID == "" || r1.CredentialFingerprint == "" || r1.EngineUID == "" {
		t.Fatalf("incomplete result: %+v", r1)
	}
	firstPID := readRegistryFile(t, dataDir)[r1.EngineUID].PID
	if firstPID == 0 {
		t.Fatalf("engine registry row missing after first bootstrap")
	}
	if !waitFor(3*time.Second, func() bool { return h1.be.alive(firstPID) }) {
		t.Fatalf("engine process never became alive (pid=%d)", firstPID)
	}
	tok1, err := os.ReadFile(r1.TokenFile)
	if err != nil {
		t.Fatalf("token file: %v", err)
	}

	// Simulated vknode restart: everything in memory is gone; only the
	// state dir and the native-process registry survive.
	h2 := newBootstrapHarness(t, dataDir, &launches)
	r2, err := Bootstrap(ctx, opts(h2))
	if err != nil {
		t.Fatalf("Bootstrap #2 (after restart): %v", err)
	}

	// Exactly one stable node name.
	if r2.NodeName != r1.NodeName {
		t.Errorf("node name changed across restart: %q -> %q", r1.NodeName, r2.NodeName)
	}
	if len(dks.names) != 1 {
		t.Errorf("DKS saw %d distinct node names, want 1: %v", len(dks.names), dks.names)
	}
	// Exactly one sphere peer ID.
	if r2.PeerID != r1.PeerID {
		t.Errorf("sphere peer ID changed across restart: %q -> %q", r1.PeerID, r2.PeerID)
	}
	if len(sphere.peers) != 1 {
		t.Errorf("sphere saw %d distinct peer IDs, want 1: %v", len(sphere.peers), sphere.peers)
	}
	// Exactly one credential set: no second scoped token minted, same bytes on disk.
	if dks.joins != 1 {
		t.Errorf("DKS Join called %d times, want 1 (restart must reuse the scoped credential)", dks.joins)
	}
	if r2.CredentialFingerprint != r1.CredentialFingerprint {
		t.Errorf("credential fingerprint changed: %q -> %q", r1.CredentialFingerprint, r2.CredentialFingerprint)
	}
	tok2, err := os.ReadFile(r2.TokenFile)
	if err != nil {
		t.Fatalf("token file after restart: %v", err)
	}
	if string(tok1) != string(tok2) {
		t.Errorf("token file rewritten with different credential across restart")
	}
	// Exactly one native-process engine registry entry, same process adopted.
	if launches != 1 {
		t.Errorf("engine launched %d times, want 1 (restart must adopt)", launches)
	}
	reg := readRegistryFile(t, dataDir)
	if len(reg) != 1 {
		t.Errorf("engine registry has %d rows, want 1: %+v", len(reg), reg)
	}
	if r2.EngineUID != r1.EngineUID {
		t.Errorf("engine UID changed: %q -> %q", r1.EngineUID, r2.EngineUID)
	}
	if reg[r2.EngineUID].PID != firstPID {
		t.Errorf("engine PID changed across restart: %d -> %d", firstPID, reg[r2.EngineUID].PID)
	}
	if !r2.State.WorkerReady() {
		t.Errorf("after restart WorkerReady=false; state=%+v", r2.State)
	}
	// Phases are tracked independently; model readiness belongs to S451.5
	// and is never claimed by bootstrap.
	if r2.State.Done(PhaseModelReady) {
		t.Errorf("bootstrap must not claim model_ready")
	}
	if !r2.State.Done(PhaseDKSJoined) || !r2.State.Done(PhaseSphereJoined) {
		t.Errorf("expected dks_joined and sphere_joined done: %+v", r2.State.Phases)
	}
	// The persisted state is what a reboot reads back.
	st, err := LoadEnrollState(stateDir)
	if err != nil {
		t.Fatalf("LoadEnrollState: %v", err)
	}
	if st.NodeName != r1.NodeName || st.PeerID != r1.PeerID || st.MachineID != machineID {
		t.Errorf("persisted state drifted: %+v", st)
	}
}

func TestBootstrap_PartialFailureIsResumableAndNotReady(t *testing.T) {
	ctx := context.Background()
	stateDir := filepath.Join(t.TempDir(), "state")
	dataDir := filepath.Join(t.TempDir(), "vk-ollama")

	dks := &fakeDKS{}
	sphere := &fakeSphere{fail: errors.New("sphere unreachable")}
	engine := helperEngineSpec(t)
	launches := 0
	h := newBootstrapHarness(t, dataDir, &launches)
	opts := BootstrapOptions{
		MachineID:  "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
		PairingID:  "pairing-1",
		StateDir:   stateDir,
		Engine:     h.be,
		EngineSpec: engine,
		DKS:        dks,
		Sphere:     sphere,
	}

	r1, err := Bootstrap(ctx, opts)
	if err == nil {
		t.Fatal("expected sphere-join failure to surface")
	}
	if r1 == nil {
		t.Fatal("partial result must be returned alongside the error")
	}
	t.Cleanup(func() { _ = h.be.Delete(ctx, r1.EnginePod) })
	if r1.State.WorkerReady() {
		t.Errorf("partial failure must never advertise full readiness: %+v", r1.State)
	}
	if !r1.State.Done(PhaseDKSJoined) || r1.State.Done(PhaseSphereJoined) {
		t.Errorf("phases: want dks_joined only, got %+v", r1.State.Phases)
	}
	if r1.State.LastError == "" {
		t.Errorf("LastError not recorded")
	}

	// Resume: sphere is back. DKS must not be re-joined; the engine must
	// not be relaunched; the result becomes ready.
	sphere.fail = nil
	r2, err := Bootstrap(ctx, opts)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if dks.joins != 1 {
		t.Errorf("resume re-joined DKS (%d joins)", dks.joins)
	}
	if launches != 1 {
		t.Errorf("resume relaunched engine (%d launches)", launches)
	}
	if sphere.joins != 1 {
		t.Errorf("sphere joins = %d, want 1", sphere.joins)
	}
	if !r2.State.WorkerReady() {
		t.Errorf("resumed bootstrap not ready: %+v", r2.State)
	}
	if r2.State.LastError != "" {
		t.Errorf("LastError should clear on success: %q", r2.State.LastError)
	}
	if sphere.last.NodeName != r2.NodeName || sphere.last.PeerID != r2.PeerID {
		t.Errorf("sphere join request mismatch: %+v vs result %+v", sphere.last, r2)
	}
	if want := fmt.Sprintf("http://127.0.0.1:%d", engine.Port); sphere.last.EngineURL != want {
		t.Errorf("sphere EngineURL = %q, want %q (watcher OllamaURL must point at the engine)", sphere.last.EngineURL, want)
	}
}

func TestBootstrap_RepairInvalidatesStaleDKSJoin(t *testing.T) {
	ctx := context.Background()
	stateDir := filepath.Join(t.TempDir(), "state")
	dataDir := filepath.Join(t.TempDir(), "vk-ollama")

	dks := &fakeDKS{}
	sphere := &fakeSphere{}
	engine := helperEngineSpec(t)
	launches := 0
	h := newBootstrapHarness(t, dataDir, &launches)
	opts := BootstrapOptions{
		MachineID:  "abababababababababababababababababababababababababababababababab",
		PairingID:  "pairing-1",
		StateDir:   stateDir,
		Engine:     h.be,
		EngineSpec: engine,
		DKS:        dks,
		Sphere:     sphere,
	}
	r1, err := Bootstrap(ctx, opts)
	if err != nil {
		t.Fatalf("Bootstrap #1: %v", err)
	}
	t.Cleanup(func() { _ = h.be.Delete(ctx, r1.EnginePod) })

	// A re-pair against a different satellite must not ride the cached
	// join (Sprint 482 45a78924): the DKS phase is redone, the node name
	// and peer ID stay the same physical-host identity.
	opts.PairingID = "pairing-2"
	r2, err := Bootstrap(ctx, opts)
	if err != nil {
		t.Fatalf("Bootstrap #2: %v", err)
	}
	if dks.joins != 2 {
		t.Errorf("DKS joins = %d, want 2 after re-pair", dks.joins)
	}
	if sphere.joins != 2 {
		t.Errorf("sphere joins = %d, want 2 after re-pair", sphere.joins)
	}
	if r2.CredentialFingerprint == r1.CredentialFingerprint {
		t.Errorf("re-pair must mint a fresh scoped credential")
	}
	if r2.NodeName != r1.NodeName || r2.PeerID != r1.PeerID {
		t.Errorf("physical-host identity must survive re-pair: %+v vs %+v", r1, r2)
	}
	if launches != 1 {
		t.Errorf("re-pair relaunched engine (%d launches)", launches)
	}
}

func TestBootstrap_RequiresPairingIdentity(t *testing.T) {
	_, err := Bootstrap(context.Background(), BootstrapOptions{
		MachineID: "abababababababababababababababab",
		StateDir:  t.TempDir(),
		DKS:       &fakeDKS{},
		Sphere:    &fakeSphere{},
	})
	if err == nil {
		t.Fatal("bootstrap without a pairing identity must fail closed")
	}
}

func TestBootstrap_RefusesForeignMachineState(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	st := &EnrollState{MachineID: "1111", NodeName: "host-1111-vk-ollama"}
	if err := SaveEnrollState(stateDir, st); err != nil {
		t.Fatal(err)
	}
	_, err := Bootstrap(context.Background(), BootstrapOptions{
		MachineID: "2222",
		StateDir:  stateDir,
		DKS:       &fakeDKS{},
		Sphere:    &fakeSphere{},
	})
	if err == nil {
		t.Fatal("expected refusal when the state dir belongs to another machine_id")
	}
}

func TestBootstrap_NodeNameKeyedByMachineIDWithHostnameFallback(t *testing.T) {
	got, err := DeriveNodeName("", "0123456789abcdef0123456789abcdef", "ignored.local", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "host-0123456789ab-vk-ollama" {
		t.Errorf("machine_id-keyed name = %q", got)
	}
	got, err = DeriveNodeName("", "", "My-Worker.Local", "vk-ollama")
	if err != nil {
		t.Fatal(err)
	}
	if got != "my-worker-vk-ollama" {
		t.Errorf("hostname-fallback name = %q", got)
	}
	got, err = DeriveNodeName("geekom", "deadbeef", "x", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "geekom-vk-ollama" {
		t.Errorf("operator override = %q", got)
	}
	if _, err := DeriveNodeName("", "", "", ""); err == nil {
		t.Error("no identity at all must be an error")
	}
}
