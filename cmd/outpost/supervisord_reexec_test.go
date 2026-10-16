package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseSupervisordPid(t *testing.T) {
	for _, tc := range []struct {
		in      string
		pid     int
		reexecs bool
	}{
		{"123", 123, false}, // pre-marker init
		{"123\n", 123, false},
		{"123\n" + initReexecMarker + "\n", 123, true},
		{"", 0, false},
	} {
		pid, re := parseSupervisordPid(tc.in)
		if pid != tc.pid || re != tc.reexecs {
			t.Errorf("parse(%q) = %d,%v want %d,%v", tc.in, pid, re, tc.pid, tc.reexecs)
		}
	}
}

func TestWatchBinarySwapDetectsAtomicRename(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "outpost")
	if err := os.WriteFile(bin, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan bool, 1)
	go func() {
		done <- watchBinarySwap(ctx, []string{bin, filepath.Join(dir, "absent")}, 10*time.Millisecond, fileIdentity)
	}()
	time.Sleep(50 * time.Millisecond)
	// Rewriting in place is the same file; only a swap counts.
	if err := os.WriteFile(bin, []byte("old2"), 0o755); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("in-place write reported as a swap")
	default:
	}
	cand := bin + ".upgrading"
	if err := os.WriteFile(cand, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(cand, bin); err != nil {
		t.Fatal(err)
	}
	if !<-done {
		t.Fatal("atomic rename over the binary not reported as a swap")
	}
}

func TestWatchBinarySwapStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if watchBinarySwap(ctx, []string{os.Args[0]}, time.Hour, fileIdentity) {
		t.Fatal("canceled watch reported a swap")
	}
}

func TestSuperviseUntilSwapStopsChildrenOnSwap(t *testing.T) {
	stopped := false
	run := func(ctx context.Context) error {
		<-ctx.Done() // the fake manager: runs until told to stop its children
		stopped = true
		return nil
	}
	swapped, err := superviseUntilSwap(context.Background(), run, func(context.Context) bool { return true })
	if !swapped || err != nil || !stopped {
		t.Fatalf("swapped=%v err=%v stopped=%v, want true,nil,true", swapped, err, stopped)
	}
}

func TestSuperviseUntilSwapShutdownIsNotASwap(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	swapped, _ := superviseUntilSwap(ctx, func(ctx context.Context) error { <-ctx.Done(); return nil },
		func(ctx context.Context) bool { <-ctx.Done(); return false })
	if swapped {
		t.Fatal("shutdown reported as a swap")
	}
}

func TestRunInitLoopReexecsOnSwap(t *testing.T) {
	cycles, execs := 0, 0
	cycle := func(context.Context) (bool, error) { cycles++; return cycles == 1, nil }
	// A failed exec must fall back to supervising again, not exit init.
	err := runInitLoop(context.Background(), cycle, func() error { execs++; return errors.New("exec failed") })
	if err != nil || cycles != 2 || execs != 1 {
		t.Fatalf("err=%v cycles=%d execs=%d, want nil,2,1", err, cycles, execs)
	}
	cycles, execs = 0, 0
	err = runInitLoop(context.Background(), cycle, func() error { execs++; return errInitHandedOff })
	if err != nil || cycles != 1 || execs != 1 {
		t.Fatalf("handed-off: err=%v cycles=%d execs=%d, want nil,1,1", err, cycles, execs)
	}
}

type fakeInit struct {
	pid      int
	reexecs  bool
	alive    map[int]bool
	outpost  bool
	stopped  []int
	spawned  int
	reclaim  int // pid a service manager writes after the old init exits
	exitOnTm bool
}

func (f *fakeInit) handoff(goos string) initHandoff {
	return initHandoff{
		readPid: func() (int, bool, error) {
			if f.pid == 0 {
				return 0, false, os.ErrNotExist
			}
			return f.pid, f.reexecs, nil
		},
		alive:     func(pid int) bool { return f.alive[pid] },
		isOutpost: func(int) bool { return f.outpost },
		stop: func(pid int) error {
			f.stopped = append(f.stopped, pid)
			if f.exitOnTm {
				f.alive[pid] = false
				f.pid = f.reclaim
				if f.reclaim != 0 {
					f.alive[f.reclaim] = true
				}
			}
			return nil
		},
		spawn:       func() error { f.spawned++; return nil },
		goos:        goos,
		exitWait:    20 * time.Millisecond,
		reclaimWait: 20 * time.Millisecond,
		poll:        time.Millisecond,
	}
}

func TestInitHandoffRestartsLegacyInit(t *testing.T) {
	f := &fakeInit{pid: 4242, alive: map[int]bool{4242: true}, outpost: true, exitOnTm: true}
	out := f.handoff("darwin").run()
	if len(f.stopped) != 1 || f.stopped[0] != 4242 || f.spawned != 1 {
		t.Fatalf("stopped=%v spawned=%d, want [4242],1; out=%v", f.stopped, f.spawned, out)
	}
	if !strings.Contains(strings.Join(out, "\n"), "started a new outpost supervisord") {
		t.Fatalf("report = %v", out)
	}
}

func TestInitHandoffDefersToServiceManager(t *testing.T) {
	f := &fakeInit{pid: 4242, alive: map[int]bool{4242: true}, outpost: true, exitOnTm: true, reclaim: 5151}
	out := f.handoff("linux").run()
	if f.spawned != 0 || !strings.Contains(strings.Join(out, "\n"), "service manager (pid 5151)") {
		t.Fatalf("spawned=%d out=%v, want the service manager's init kept", f.spawned, out)
	}
}

func TestInitHandoffLeavesOthersAlone(t *testing.T) {
	for name, f := range map[string]*fakeInit{
		"reexec-capable": {pid: 4242, reexecs: true, alive: map[int]bool{4242: true}, outpost: true},
		"foreign pid":    {pid: 4242, alive: map[int]bool{4242: true}, outpost: false},
		"not running":    {pid: 4242, alive: map[int]bool{}, outpost: true},
		"no pidfile":     {alive: map[int]bool{}},
		"windows":        {pid: 4242, alive: map[int]bool{4242: true}, outpost: true},
	} {
		goos := "darwin"
		if name == "windows" {
			goos = "windows"
		}
		f.handoff(goos).run()
		if len(f.stopped) != 0 || f.spawned != 0 {
			t.Errorf("%s: stopped=%v spawned=%d, want no action", name, f.stopped, f.spawned)
		}
	}
}

func TestInitHandoffDoesNotSpawnWhenOldInitSurvives(t *testing.T) {
	f := &fakeInit{pid: 4242, alive: map[int]bool{4242: true}, outpost: true} // ignores the signal
	out := f.handoff("darwin").run()
	if f.spawned != 0 || !strings.Contains(strings.Join(out, "\n"), "did not exit") {
		t.Fatalf("spawned=%d out=%v, want no second init beside a live one", f.spawned, out)
	}
}

func TestInitStatusWarnings(t *testing.T) {
	if w := initStatusWarnings(true, nil); len(w) != 0 {
		t.Fatalf("healthy init warned: %v", w)
	}
	w := initStatusWarnings(true, errors.New("bashy supervisord is not running"))
	if len(w) != 1 || !strings.Contains(w[0], "bashy supervisord service monitor is not") {
		t.Fatalf("missing monitor warning: %v", w)
	}
	if w := initStatusWarnings(false, nil); len(w) != 1 || !strings.Contains(w[0], "predates") {
		t.Fatalf("missing legacy-init warning: %v", w)
	}
}
