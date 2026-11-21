package main

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/service"

	"github.com/qiangli/outpost/internal/agent"
)

func TestPodmanCapabilityAutostartOffDoesNotStartMachine(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "pm-off-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "p.sock")

	var starts atomic.Int32
	c := &podmanCapability{
		Apps:   agent.NewAppRegistry(),
		Detect: func() agent.BuiltinTarget { return agent.BuiltinTarget{Name: "podman", Scheme: "unix", Socket: sock} },
		EnsureMachine: func(context.Context) (string, error) {
			starts.Add(1)
			return sock, nil
		},
	}
	if err := c.Activate(context.Background(), service.Config{}); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if starts.Load() != 0 {
		t.Fatalf("EnsureMachine called %d times with autostart_machine unset", starts.Load())
	}
}

func TestPodmanCapabilityAutostartNonBlockingSingleFlightAndRecovery(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "pm-auto-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "p.sock")

	var mu sync.Mutex
	target := agent.BuiltinTarget{Name: "podman", Scheme: "unix", Socket: sock}
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	var starts atomic.Int32

	apps := agent.NewAppRegistry()
	c := &podmanCapability{
		Apps: apps,
		Detect: func() agent.BuiltinTarget {
			mu.Lock()
			defer mu.Unlock()
			return target
		},
		EnsureMachine: func(ctx context.Context) (string, error) {
			if starts.Add(1) == 1 {
				close(entered)
			}
			select {
			case <-release:
			case <-ctx.Done():
				return "", ctx.Err()
			}
			defer close(done)
			return sock, nil
		},
	}

	reg := service.NewRegistry()
	if err := reg.Register(capPodman, c); err != nil {
		t.Fatal(err)
	}
	p := service.NewInternal("outpost", reg)
	tcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tcpAddr := tcpLn.Addr().String()
	_ = tcpLn.Close() // closed initially; opened when Klatch comes up inside the VM

	unit := fleet.Service{
		Name:       "podman",
		Kind:       fleet.KindService,
		Provider:   fleet.ServiceProviderInternal,
		Capability: capPodman,
		Owner:      "outpost",
		Enabled:    true,
		Settings: map[string]string{
			"autostart_machine": "true",
			"ready_tcp":         tcpAddr,
		},
	}
	ctx := context.Background()

	// 1. Activate returns nil immediately without waiting for EnsureMachine,
	//    resting Inactive with 'unavailable: starting podman machine...'.
	if err := p.Activate(ctx, unit, service.Runtime{}); err != nil {
		t.Fatalf("non-blocking Activate failed: %v", err)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("background EnsureMachine did not start")
	}
	st := p.Status("podman")
	if st.State != service.Inactive || st.Reason != "unavailable: starting podman machine..." {
		t.Fatalf("status while starting = %+v, want Inactive 'unavailable: starting podman machine...'", st)
	}

	// 2. Re-probing Health while EnsureMachine is in flight neither kills it nor
	//    spawns a second start (single-flight).
	if err := p.Health(ctx, "podman"); err == nil || !strings.Contains(err.Error(), "starting podman machine") {
		t.Fatalf("Health while starting = %v, want unavailable starting reason", err)
	}
	if got := starts.Load(); got != 1 {
		t.Fatalf("single-flight violated: starts = %d, want 1", got)
	}

	// 3. Machine starts and socket appears, before ready_tcp opens -> stays unavailable waiting for ready_tcp.
	sockLn, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer sockLn.Close()
	mu.Lock()
	target.Available = true
	mu.Unlock()
	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("EnsureMachine did not complete")
	}
	// Let the goroutine record its post-start state briefly.
	time.Sleep(20 * time.Millisecond)

	if err := p.Activate(ctx, unit, service.Runtime{}); err != nil {
		t.Fatalf("Activate before ready_tcp: %v", err)
	}
	if st := p.Status("podman"); st.State != service.Inactive || !strings.Contains(st.Reason, tcpAddr) {
		t.Fatalf("status before ready_tcp = %+v, want waiting for %s", st, tcpAddr)
	}

	// 4. Container listener (e.g. Klatch) opens ready_tcp -> Health & Activate recover to Active!
	klatchLn, err := net.Listen("tcp", tcpAddr)
	if err != nil {
		t.Skipf("loopback port %s raced with another process: %v", tcpAddr, err)
	}
	defer klatchLn.Close()

	if err := p.Health(ctx, "podman"); err != nil {
		t.Fatalf("Health once socket + ready_tcp up: %v", err)
	}
	if err := p.Activate(ctx, unit, service.Runtime{}); err != nil {
		t.Fatalf("Activate once ready: %v", err)
	}
	if st := p.Status("podman"); st.State != service.Active || st.Reason != "" {
		t.Fatalf("status after recovery = %+v, want Active", st)
	}
	if len(apps.Names()) == 0 {
		t.Fatal("podman app was not registered after recovery")
	}
}

func TestPodmanCapabilityAutostartNeverInitsMissingMachine(t *testing.T) {
	if runtime.GOOS != "windows" {
		// Direct test of podmanMachineToStart over a fake podman script that
		// reports zero machines and fails if `machine init` or `machine start`
		// is ever called.
		tmp := t.TempDir()
		logPath := filepath.Join(tmp, "calls.txt")
		fakePodman := filepath.Join(tmp, "fake-podman.sh")
		script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + logPath + "'\n" +
			"if [ \"$1\" = \"machine\" ] && [ \"$2\" = \"list\" ]; then printf '[]\\n'; exit 0; fi\n" +
			"exit 99\n"
		if err := os.WriteFile(fakePodman, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		_, err := podmanMachineToStart(context.Background(), []string{fakePodman})
		if !errors.Is(err, errNoPodmanMachine) {
			t.Fatalf("podmanMachineToStart([]) err = %v, want errNoPodmanMachine", err)
		}
		raw, _ := os.ReadFile(logPath)
		if strings.Contains(string(raw), "init") || strings.Contains(string(raw), "start") {
			t.Fatalf("missing machine invoked init/start: %s", string(raw))
		}
	}

	dir, err := os.MkdirTemp("/tmp", "pm-nomach-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "p.sock")

	now := time.Date(2026, 11, 20, 16, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	var calls atomic.Int32
	done := make(chan struct{}, 2)
	c := &podmanCapability{
		Apps:   agent.NewAppRegistry(),
		Detect: func() agent.BuiltinTarget { return agent.BuiltinTarget{Name: "podman", Scheme: "unix", Socket: sock} },
		Now: func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return now
		},
		EnsureMachine: func(context.Context) (string, error) {
			calls.Add(1)
			done <- struct{}{}
			return "", errNoPodmanMachine
		},
	}
	cfg := service.Config{Settings: map[string]string{"autostart_machine": "true"}}
	if err := c.Activate(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	<-done
	time.Sleep(20 * time.Millisecond)

	if !strings.Contains(c.Unavailable(), "no podman machine exists") {
		t.Fatalf("Unavailable = %q, want no podman machine exists", c.Unavailable())
	}
	// Immediate Health re-probe must not re-run EnsureMachine before recheck interval.
	if err := c.Health(context.Background()); err != nil {
		t.Fatalf("Health while no machine: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("EnsureMachine called %d times within no-machine cooldown, want 1", got)
	}
}

func TestPodmanCapabilityAutostartBackoffPreventsRetryStorm(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "pm-backoff-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "p.sock")

	var clockMu sync.Mutex
	now := time.Date(2026, 11, 20, 16, 0, 0, 0, time.UTC)
	advance := func(d time.Duration) {
		clockMu.Lock()
		now = now.Add(d)
		clockMu.Unlock()
	}

	var calls atomic.Int32
	attemptDone := make(chan struct{}, 4)
	c := &podmanCapability{
		Apps:        agent.NewAppRegistry(),
		Detect:      func() agent.BuiltinTarget { return agent.BuiltinTarget{Name: "podman", Scheme: "unix", Socket: sock} },
		BackoffBase: 10 * time.Second,
		Now: func() time.Time {
			clockMu.Lock()
			defer clockMu.Unlock()
			return now
		},
		EnsureMachine: func(context.Context) (string, error) {
			calls.Add(1)
			attemptDone <- struct{}{}
			return "", errors.New("wsl exit 1")
		},
	}
	cfg := service.Config{Settings: map[string]string{"autostart_machine": "true"}}

	// Attempt 1 fails -> backoff = 10s.
	_ = c.Activate(context.Background(), cfg)
	<-attemptDone
	time.Sleep(20 * time.Millisecond)
	if !strings.Contains(c.Unavailable(), "retry in 10s") {
		t.Fatalf("after 1st failure Unavailable = %q, want retry in 10s", c.Unavailable())
	}

	// Probe at +5s: still in backoff -> no second call.
	advance(5 * time.Second)
	_ = c.Health(context.Background())
	if got := calls.Load(); got != 1 {
		t.Fatalf("during backoff calls = %d, want 1", got)
	}

	// Probe at +11s: backoff elapsed -> attempt 2 runs, fails -> backoff doubles to 20s.
	advance(6 * time.Second)
	_ = c.Health(context.Background())
	<-attemptDone
	time.Sleep(20 * time.Millisecond)
	if got := calls.Load(); got != 2 {
		t.Fatalf("after backoff elapsed calls = %d, want 2", got)
	}
	if !strings.Contains(c.Unavailable(), "retry in 20s") {
		t.Fatalf("after 2nd failure Unavailable = %q, want retry in 20s", c.Unavailable())
	}
}

func TestPodmanCommandEnvInjectsWindowsSystem32(t *testing.T) {
	// 1. Minimal Task Scheduler PATH lacking System32 -> appended so wsl.exe resolves.
	got := podmanCommandEnv("windows", []string{
		`SystemRoot=C:\Windows`,
		`Path=C:\App\outpost`,
	})
	if !slices.Contains(got, `Path=C:\App\outpost;C:\Windows\System32`) {
		t.Fatalf("windows env = %v, want C:\\Windows\\System32 appended to Path", got)
	}

	// 2. Case-insensitive idempotence when System32 is already present.
	already := []string{
		`SystemRoot=C:\Windows`,
		`PATH=C:\WINDOWS\system32\;C:\App\outpost`,
	}
	got2 := podmanCommandEnv("windows", already)
	if !slices.Equal(got2, already) {
		t.Fatalf("duplicate System32 added: got %v, want %v", got2, already)
	}

	// 3. Missing PATH altogether -> added cleanly.
	got3 := podmanCommandEnv("windows", []string{`SystemRoot=D:\WinNT`})
	if !slices.Contains(got3, `PATH=D:\WinNT\System32`) {
		t.Fatalf("missing PATH env = %v, want PATH=D:\\WinNT\\System32", got3)
	}

	// 4. Non-Windows left untouched.
	unix := []string{"PATH=/usr/local/bin:/usr/bin"}
	if got4 := podmanCommandEnv("linux", unix); !slices.Equal(got4, unix) {
		t.Fatalf("linux env modified: %v", got4)
	}
}
