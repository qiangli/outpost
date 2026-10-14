package shard

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

// errGone stands in for the ESRCH a kill of an already-exited PID returns.
var errGone = errors.New("process already gone")

// Teardown kills exactly the PIDs this outpost tracked for the binary —
// never a sibling, never by pattern: an untracked PID (even one running
// the same binary) must not be signaled.
func TestKillTrackedPrima_OnlyTrackedPIDs(t *testing.T) {
	dir := t.TempDir()
	binA := filepath.Join(dir, "prima-server")
	binB := filepath.Join(dir, "prima-worker")

	old := killPID
	var mu sync.Mutex
	var got []int
	killPID = func(pid int) error {
		mu.Lock()
		got = append(got, pid)
		mu.Unlock()
		return nil
	}
	t.Cleanup(func() { killPID = old })

	TrackPrimaProc(111, binA)
	TrackPrimaProc(222, binA)
	TrackPrimaProc(333, binB)

	killed := KillTrackedPrima(binA)
	if len(killed) != 2 {
		t.Fatalf("killed %v for binA, want exactly [111 222]", killed)
	}
	mu.Lock()
	seen := map[int]bool{}
	for _, p := range got {
		seen[p] = true
	}
	mu.Unlock()
	if !seen[111] || !seen[222] || seen[333] {
		t.Errorf("signaled PIDs = %v, want exactly {111 222} (333 is binB's)", got)
	}
	if len(TrackedPrimaPIDs(binA)) != 0 {
		t.Errorf("binA PIDs still tracked after kill: %v", TrackedPrimaPIDs(binA))
	}
	// binB untouched — still tracked, never signaled.
	if pids := TrackedPrimaPIDs(binB); len(pids) != 1 || pids[0] != 333 {
		t.Errorf("binB tracking disturbed: %v", pids)
	}
	UntrackPrimaProc(333) // test hygiene: the global registry is shared
}

// A kill error (process already gone) still forgets the PID: a stale entry
// must never be signaled again after its number is recycled.
func TestKillTrackedPrima_KillFailureStillUntracks(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "prima")
	old := killPID
	killPID = func(int) error { return errGone }
	t.Cleanup(func() { killPID = old })

	TrackPrimaProc(444, bin)
	if killed := KillTrackedPrima(bin); len(killed) != 0 {
		t.Errorf("failed kill reported as killed: %v", killed)
	}
	if pids := TrackedPrimaPIDs(bin); len(pids) != 0 {
		t.Errorf("failed-kill PID still tracked: %v", pids)
	}
}

// Empty binary path: no-op, no signals.
func TestKillTrackedPrima_EmptyBinNoop(t *testing.T) {
	old := killPID
	killPID = func(pid int) error {
		t.Errorf("killPID(%d) must never fire for an empty bin path", pid)
		return nil
	}
	t.Cleanup(func() { killPID = old })
	if killed := KillTrackedPrima(""); len(killed) != 0 {
		t.Errorf("empty bin killed %v", killed)
	}
}

// Start tracks the exact PID it launched; Stop forgets it once reaped.
func TestSession_TracksPidAndStopUntracks(t *testing.T) {
	f := newFake()
	plan, err := ring2().PlanFor(0)
	if err != nil {
		t.Fatal(err)
	}
	bin := writeStub(t)
	sess, err := Start(context.Background(), f, plan, LaunchConfig{BinaryPath: bin, ModelPath: "/m.gguf"})
	if err != nil {
		t.Fatal(err)
	}
	if sess.Pid() <= 0 {
		t.Fatalf("session has no tracked PID: %d", sess.Pid())
	}
	found := false
	for _, p := range TrackedPrimaPIDs(bin) {
		if p == sess.Pid() {
			found = true
		}
	}
	if !found {
		t.Errorf("PID %d not tracked for %s", sess.Pid(), bin)
	}
	sess.Stop()
	if pids := TrackedPrimaPIDs(bin); len(pids) != 0 {
		t.Errorf("reaped PID still tracked: %v", pids)
	}
}

// Form reaps this outpost's stale tracked prima for the role bins BEFORE
// provisioning — the stuck rank that holds the ports (and the Windows
// binary lock) is gone before the re-fetch replaces anything.
func TestForm_ReapsStaleTrackedBeforeProvision(t *testing.T) {
	f := newFake()
	dir := t.TempDir()
	serverBin := writeStub(t)
	workerBin := filepath.Join(dir, "prima-worker")
	m := NewManager(ManagerConfig{
		Self: ShardPeer{Host: "leader", PeerID: "self"}, Forwarder: f, Peers: &fakePeers{},
		Bins: ServeBins{ServerBin: serverBin, WorkerBin: workerBin},
	})
	provisioned := false
	m.provision = func(context.Context, string) (string, error) {
		provisioned = true
		return "/m.gguf", nil
	}

	old := killPID
	var mu sync.Mutex
	var got []int
	killPID = func(pid int) error {
		mu.Lock()
		got = append(got, pid)
		mu.Unlock()
		return nil
	}
	t.Cleanup(func() { killPID = old })

	TrackPrimaProc(424242, workerBin) // a stuck worker from a previous form
	ring := ring2()
	sc := ServeConfig{Model: "big-model", ServerBin: serverBin, WorkerBin: workerBin, APIPort: 9999}
	if err := m.Form(context.Background(), &ring, 0, sc); err != nil {
		t.Fatalf("Form: %v", err)
	}
	if !provisioned {
		t.Error("provision never ran")
	}
	mu.Lock()
	seen := map[int]bool{}
	for _, p := range got {
		seen[p] = true
	}
	mu.Unlock()
	if !seen[424242] {
		t.Errorf("stale tracked PID 424242 not reaped before re-provision (signaled: %v)", got)
	}
	if pids := TrackedPrimaPIDs(workerBin); len(pids) != 0 {
		t.Errorf("stale PID still tracked after Form: %v", pids)
	}
	m.Stop() // reaps the stub this Form launched
}

// Stop reaps orphaned tracked prima that outlived their session — the
// teardown path that frees a stuck Windows rank's binary lock.
func TestStop_ReapsOrphanedTracked(t *testing.T) {
	dir := t.TempDir()
	serverBin := filepath.Join(dir, "prima-server")
	workerBin := filepath.Join(dir, "prima-worker")
	m := NewManager(ManagerConfig{
		Self: ShardPeer{Host: "leader", PeerID: "self"}, Peers: &fakePeers{},
		Bins: ServeBins{ServerBin: serverBin, WorkerBin: workerBin},
	})
	old := killPID
	var mu sync.Mutex
	var got []int
	killPID = func(pid int) error {
		mu.Lock()
		got = append(got, pid)
		mu.Unlock()
		return nil
	}
	t.Cleanup(func() { killPID = old })

	TrackPrimaProc(515151, serverBin) // orphan: no live session references it
	m.Stop()
	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, p := range got {
		if p == 515151 {
			found = true
		}
	}
	if !found {
		t.Errorf("teardown did not reap orphaned tracked PID 515151 (signaled: %v)", got)
	}
	if pids := TrackedPrimaPIDs(serverBin); len(pids) != 0 {
		t.Errorf("orphan still tracked after Stop: %v", pids)
	}
}
