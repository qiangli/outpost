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
	pid       int
	reexecs   bool
	alive     map[int]bool
	outpost   bool
	notSupd   bool
	stopped   []int
	spawned   int
	reclaim   int // pid a service manager writes after the old init exits
	exitOnTm  bool
	discovery func() (initProbe, bool, string)
}

func (f *fakeInit) handoff() initHandoff {
	discover := f.discovery
	if discover == nil {
		discover = func() (initProbe, bool, string) {
			if f.pid == 0 || !f.alive[f.pid] {
				return initProbe{}, false, "no pidfile"
			}
			return initProbe{pid: f.pid, reexecs: f.reexecs, source: "pidfile test"}, true, ""
		}
	}
	return initHandoff{
		discover:      discover,
		alive:         func(pid int) bool { return f.alive[pid] },
		isOutpost:     func(int) bool { return f.outpost },
		isSupervisord: func(int) bool { return !f.notSupd },
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
		exitWait:    20 * time.Millisecond,
		reclaimWait: 20 * time.Millisecond,
		poll:        time.Millisecond,
	}
}

func TestInitHandoffRestartsLegacyInit(t *testing.T) {
	f := &fakeInit{pid: 4242, alive: map[int]bool{4242: true}, outpost: true, exitOnTm: true}
	out := f.handoff().run()
	if len(f.stopped) != 1 || f.stopped[0] != 4242 || f.spawned != 1 {
		t.Fatalf("stopped=%v spawned=%d, want [4242],1; out=%v", f.stopped, f.spawned, out)
	}
	if !strings.Contains(strings.Join(out, "\n"), "handed off pid 4242; started a new outpost supervisord") {
		t.Fatalf("report = %v", out)
	}
}

func TestInitHandoffDefersToServiceManager(t *testing.T) {
	f := &fakeInit{pid: 4242, alive: map[int]bool{4242: true}, outpost: true, exitOnTm: true, reclaim: 5151}
	out := f.handoff().run()
	if f.spawned != 0 || !strings.Contains(strings.Join(out, "\n"), "service manager (pid 5151)") {
		t.Fatalf("spawned=%d out=%v, want the service manager's init kept", f.spawned, out)
	}
}

func TestInitHandoffLeavesOthersAlone(t *testing.T) {
	for name, tc := range map[string]struct {
		f    *fakeInit
		want string
	}{
		"reexec-capable":  {&fakeInit{pid: 4242, reexecs: true, alive: map[int]bool{4242: true}, outpost: true}, "skipped: pid 4242"},
		"foreign pid":     {&fakeInit{pid: 4242, alive: map[int]bool{4242: true}, outpost: false}, "skipped: pid 4242"},
		"not supervisord": {&fakeInit{pid: 4242, alive: map[int]bool{4242: true}, outpost: true, notSupd: true}, "not an outpost supervisord invocation"},
		"not running":     {&fakeInit{pid: 4242, alive: map[int]bool{}, outpost: true}, "no running init"},
		"no pidfile":      {&fakeInit{alive: map[int]bool{}}, "no running init"},
	} {
		out := tc.f.handoff().run()
		if len(tc.f.stopped) != 0 || tc.f.spawned != 0 {
			t.Errorf("%s: stopped=%v spawned=%d, want no action", name, tc.f.stopped, tc.f.spawned)
		}
		if len(out) != 1 || !strings.Contains(out[0], tc.want) {
			t.Errorf("%s: report = %v, want one line containing %q", name, out, tc.want)
		}
	}
}

// TestInitHandoffSameOnEveryOS: the legacy-init handoff takes the same
// stop-and-reclaim path on Windows as on unix — there is no Windows-only
// "warn and leave it to the operator" branch anymore. Windows stops via
// TerminateProcess (requestStop) and re-execs by spawning a detached
// successor, so the handoff logic is identical.
func TestInitHandoffSameOnEveryOS(t *testing.T) {
	f := &fakeInit{pid: 4242, alive: map[int]bool{4242: true}, outpost: true, exitOnTm: true}
	out := f.handoff().run()
	if len(f.stopped) != 1 || f.stopped[0] != 4242 || f.spawned != 1 {
		t.Fatalf("stopped=%v spawned=%d, want [4242],1 on every OS; out=%v", f.stopped, f.spawned, out)
	}
	if !strings.Contains(strings.Join(out, "\n"), "handed off pid 4242; started a new outpost supervisord") {
		t.Fatalf("report = %v", out)
	}
}

func TestInitHandoffDoesNotSpawnWhenOldInitSurvives(t *testing.T) {
	f := &fakeInit{pid: 4242, alive: map[int]bool{4242: true}, outpost: true} // ignores the signal
	out := f.handoff().run()
	if f.spawned != 0 || !strings.Contains(strings.Join(out, "\n"), "did not exit") {
		t.Fatalf("spawned=%d out=%v, want no second init beside a live one", f.spawned, out)
	}
}

// The live failure: a pre-split init with NO pidfile anywhere. Status found
// it; the handoff must find it the same way and hand it off.
func TestInitHandoffNoPidfileStatusDiscovered(t *testing.T) {
	alive := map[int]bool{26580: true, 900: true, 901: true}
	d := initDiscovery{
		pidFiles: func() []string { return []string{"/nope/supervisord.pid", "/legacy/supervisord.pid"} },
		readFile: func(string) ([]byte, error) { return nil, os.ErrNotExist },
		alive:    func(pid int) bool { return alive[pid] },
		scan: func() ([]procEntry, error) {
			return []procEntry{
				{pid: 900, args: "/Users/u/bin/outpost supervisord status"},
				{pid: 901, args: "/usr/bin/vim supervisord"},
				{pid: 26580, args: "/Users/u/bin/outpost supervisord"},
			}, nil
		},
		self: 1,
	}
	probe, found, _ := d.find()
	if !found || probe.pid != 26580 || probe.reexecs || probe.source != "process table" {
		t.Fatalf("discovery = %+v found=%v, want pid 26580 from the process table, no reexec marker", probe, found)
	}
	f := &fakeInit{pid: 26580, alive: alive, outpost: true, exitOnTm: true, discovery: d.find}
	out := f.handoff().run()
	if len(f.stopped) != 1 || f.stopped[0] != 26580 || f.spawned != 1 {
		t.Fatalf("stopped=%v spawned=%d, want [26580],1; out=%v", f.stopped, f.spawned, out)
	}
	if !strings.Contains(strings.Join(out, "\n"), "handed off pid 26580") {
		t.Fatalf("report = %v", out)
	}
}

func TestInitDiscoveryPrefersPidfile(t *testing.T) {
	d := initDiscovery{
		pidFiles: func() []string { return []string{"/a", "/b"} },
		readFile: func(p string) ([]byte, error) {
			if p == "/a" {
				return []byte("77\n"), nil // stale
			}
			return []byte("88\n" + initReexecMarker + "\n"), nil
		},
		alive: func(pid int) bool { return pid == 88 },
		scan:  func() ([]procEntry, error) { t.Fatal("scanned despite a live pidfile"); return nil, nil },
	}
	if p, ok, _ := d.find(); !ok || p.pid != 88 || !p.reexecs || p.source != "pidfile /b" {
		t.Fatalf("got %+v ok=%v", p, ok)
	}
	d.alive = func(int) bool { return false }
	d.scan = func() ([]procEntry, error) { return nil, nil }
	if _, ok, reason := d.find(); ok || !strings.Contains(reason, "stale pidfile") {
		t.Fatalf("ok=%v reason=%q, want stale", ok, reason)
	}
}

func TestIsSupervisordInvocation(t *testing.T) {
	for args, want := range map[string]bool{
		"/x/outpost supervisord":           true,
		"outpost supervisord --foo":        true,
		"/x/outpost supervisord status":    false,
		"/x/outpost start":                 false,
		"/usr/local/bin/bashy supervisord": false,
		"outpost":                          false,
	} {
		if got := isSupervisordInvocation(args); got != want {
			t.Errorf("%q = %v, want %v", args, got, want)
		}
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
