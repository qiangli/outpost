package ollama

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGuest is the fake clock + fake loaded-model source + fake poweroff the
// watchdog tests drive. Sleep advances the clock instead of waiting, so a
// run over hours of simulated time finishes instantly.
type fakeGuest struct {
	mu       sync.Mutex
	now      time.Time
	start    time.Time
	active   func(elapsed time.Duration) bool
	powerErr []error // consumed one per PowerOff call; exhausted → nil
	powered  []time.Duration
	pushes   []string
	log      bytes.Buffer
}

func newFakeGuest() *fakeGuest {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return &fakeGuest{now: t0, start: t0, active: func(time.Duration) bool { return false }}
}

func (f *fakeGuest) config(requestID string) GuestWatchdogConfig {
	return GuestWatchdogConfig{
		RequestID:  requestID,
		Idle:       20 * time.Minute,
		MaxSession: 4 * time.Hour,
		Interval:   time.Minute,
		Retry:      30 * time.Second,
		Now: func() time.Time {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.now
		},
		Sleep: func(ctx context.Context, d time.Duration) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			f.mu.Lock()
			f.now = f.now.Add(d)
			f.mu.Unlock()
			return nil
		},
		Uptime:  func() (time.Duration, error) { return 0, nil },
		Machine: func(string) error { return nil },
		Active: func(context.Context) (bool, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.active(f.now.Sub(f.start)), nil
		},
		FinalPush: func(_ context.Context, reason string) error {
			f.pushes = append(f.pushes, reason)
			return nil
		},
		PowerOff: func() error {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.powered = append(f.powered, f.now.Sub(f.start))
			if len(f.powerErr) > 0 {
				err := f.powerErr[0]
				f.powerErr = f.powerErr[1:]
				return err
			}
			return nil
		},
		Logger: slog.New(slog.NewTextHandler(&f.log, nil)),
	}
}

func runWatchdog(t *testing.T, cfg GuestWatchdogConfig) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return NewGuestWatchdog(cfg).Run(ctx)
}

func TestGuestWatchdogNonWorkerNeverArms(t *testing.T) {
	f := newFakeGuest()
	cfg := f.config("")
	slept := false
	cfg.Sleep = func(context.Context, time.Duration) error { slept = true; return nil }
	if err := runWatchdog(t, cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if slept || len(f.powered) != 0 || len(f.pushes) != 0 {
		t.Fatalf("non-worker host armed: slept=%v powered=%v pushes=%v", slept, f.powered, f.pushes)
	}
}

func TestGuestWatchdogIdleFiresAfterThreshold(t *testing.T) {
	f := newFakeGuest()
	if err := runWatchdog(t, f.config("req-1")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(f.powered) != 1 || f.powered[0] != 20*time.Minute {
		t.Fatalf("powered at %v, want once at 20m", f.powered)
	}
	if len(f.pushes) != 1 || f.pushes[0] != GuestStopIdle {
		t.Fatalf("final pushes = %v, want [%s]", f.pushes, GuestStopIdle)
	}
	if !strings.Contains(f.log.String(), "reason=idle") {
		t.Fatalf("trigger reason not logged:\n%s", f.log.String())
	}
}

func TestGuestWatchdogActivityResetsIdle(t *testing.T) {
	f := newFakeGuest()
	// Busy for the first 50 minutes, then nothing.
	f.active = func(e time.Duration) bool { return e <= 50*time.Minute }
	if err := runWatchdog(t, f.config("req-1")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(f.powered) != 1 || f.powered[0] != 70*time.Minute {
		t.Fatalf("powered at %v, want once at 70m (last activity 50m + idle 20m)", f.powered)
	}
}

func TestGuestWatchdogMaxSessionFiresWhenBusy(t *testing.T) {
	f := newFakeGuest()
	f.active = func(time.Duration) bool { return true }
	if err := runWatchdog(t, f.config("req-1")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(f.powered) != 1 || f.powered[0] != 4*time.Hour {
		t.Fatalf("powered at %v, want once at 4h", f.powered)
	}
	if len(f.pushes) != 1 || f.pushes[0] != GuestStopMaxSession {
		t.Fatalf("final pushes = %v, want [%s]", f.pushes, GuestStopMaxSession)
	}
}

func TestGuestWatchdogMaxSessionCountsHostUptime(t *testing.T) {
	// A daemon restart must not reset the cap: the session is the host's.
	f := newFakeGuest()
	f.active = func(time.Duration) bool { return true }
	cfg := f.config("req-1")
	cfg.Uptime = func() (time.Duration, error) { return 3 * time.Hour, nil }
	if err := runWatchdog(t, cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(f.powered) != 1 || f.powered[0] != time.Hour {
		t.Fatalf("powered at %v, want once 1h after daemon start (3h uptime)", f.powered)
	}
}

func TestGuestWatchdogPoweroffErrorRetried(t *testing.T) {
	f := newFakeGuest()
	f.powerErr = []error{errors.New("operation not permitted"), errors.New("operation not permitted")}
	if err := runWatchdog(t, f.config("req-1")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := []time.Duration{20 * time.Minute, 20*time.Minute + 30*time.Second, 21 * time.Minute}
	if len(f.powered) != len(want) {
		t.Fatalf("poweroff attempts at %v, want %v", f.powered, want)
	}
	for i := range want {
		if f.powered[i] != want[i] {
			t.Fatalf("poweroff attempts at %v, want %v", f.powered, want)
		}
	}
	if got := strings.Count(f.log.String(), "operation not permitted"); got != 2 {
		t.Fatalf("poweroff error logged %d times, want 2:\n%s", got, f.log.String())
	}
	if len(f.pushes) != 1 {
		t.Fatalf("final push sent %d times, want once", len(f.pushes))
	}
}

func TestGuestWatchdogPoweroffFailureNotSwallowedOnShutdown(t *testing.T) {
	f := newFakeGuest()
	cfg := f.config("req-1")
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	cfg.PowerOff = func() error {
		attempts++
		if attempts == 3 {
			cancel()
		}
		return errors.New("operation not permitted")
	}
	err := NewGuestWatchdog(cfg).Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "operation not permitted") {
		t.Fatalf("Run = %v, want the poweroff failure surfaced", err)
	}
}

func TestGuestWatchdogFinalPushFailureStillPowersOff(t *testing.T) {
	f := newFakeGuest()
	cfg := f.config("req-1")
	cfg.FinalPush = func(context.Context, string) error { return errors.New("control plane unreachable") }
	if err := runWatchdog(t, cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(f.powered) != 1 {
		t.Fatalf("powered %d times, want 1", len(f.powered))
	}
	if !strings.Contains(f.log.String(), "control plane unreachable") {
		t.Fatalf("final push failure not logged:\n%s", f.log.String())
	}
}

func TestGuestWatchdogProbeErrorIsNotActivity(t *testing.T) {
	// A dead daemon must not keep the GPU running.
	f := newFakeGuest()
	cfg := f.config("req-1")
	cfg.Active = func(context.Context) (bool, error) { return false, errors.New("connection refused") }
	if err := runWatchdog(t, cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(f.powered) != 1 || f.powered[0] != 20*time.Minute {
		t.Fatalf("powered at %v, want once at 20m", f.powered)
	}
}

func TestGuestWatchdogEnvDurations(t *testing.T) {
	t.Setenv(GuestIdleEnv, "45m")
	t.Setenv(GuestMaxSessionEnv, "bogus")
	idle, maxSession, warn := GuestWatchdogDurations()
	if idle != 45*time.Minute || maxSession != DefaultGuestMaxSession {
		t.Fatalf("durations = %v/%v, want 45m/%v", idle, maxSession, DefaultGuestMaxSession)
	}
	if len(warn) != 1 || !strings.Contains(warn[0], GuestMaxSessionEnv) {
		t.Fatalf("warnings = %v, want one naming %s", warn, GuestMaxSessionEnv)
	}
	t.Setenv(GuestIdleEnv, "")
	t.Setenv(GuestMaxSessionEnv, "")
	idle, maxSession, warn = GuestWatchdogDurations()
	if idle != DefaultGuestIdle || maxSession != DefaultGuestMaxSession || len(warn) != 0 {
		t.Fatalf("defaults = %v/%v %v", idle, maxSession, warn)
	}
}

// dmiFiles is a fake DMI tree for EC2MachineFact: path → contents; a
// missing path reads as "file does not exist".
func dmiFiles(files map[string]string) func(string) ([]byte, error) {
	return func(path string) ([]byte, error) {
		if v, ok := files[path]; ok {
			return []byte(v), nil
		}
		return nil, fs.ErrNotExist
	}
}

func TestEC2MachineFact(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		ok    bool
	}{
		{"no DMI", nil, false},
		{"other vendor", map[string]string{dmiSysVendor: "Dell Inc.\n", dmiBoardVendor: "Dell Inc.\n"}, false},
		{"sys_vendor EC2", map[string]string{dmiSysVendor: "Amazon EC2\n"}, true},
		{"board_vendor EC2", map[string]string{dmiSysVendor: "Xen\n", dmiBoardVendor: "Amazon EC2\n"}, true},
		{"lookalike", map[string]string{dmiSysVendor: "Amazon EC2 Lookalike\n"}, false},
	}
	for _, tc := range cases {
		err := EC2MachineFact(dmiFiles(tc.files))("req-1")
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

// armed runs the watchdog with the real DMI fact over a fake DMI tree and
// reports whether it armed (i.e. ever powered off).
func armedWith(t *testing.T, requestID string, files map[string]string) (*fakeGuest, bool) {
	t.Helper()
	f := newFakeGuest()
	cfg := f.config(requestID)
	cfg.Machine = EC2MachineFact(dmiFiles(files))
	if err := runWatchdog(t, cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return f, len(f.powered) > 0
}

func TestGuestWatchdogEnvSetDMIAbsentNeverArms(t *testing.T) {
	for _, files := range []map[string]string{
		nil,
		{dmiSysVendor: "QEMU\n", dmiBoardVendor: "\n"},
	} {
		f, armed := armedWith(t, "req-1", files)
		if armed || len(f.pushes) != 0 {
			t.Fatalf("DMI %v: armed without the EC2 machine fact (powered=%v)", files, f.powered)
		}
		if got := strings.Count(f.log.String(), "refusing to arm"); got != 1 {
			t.Fatalf("DMI %v: refusal logged %d times, want once:\n%s", files, got, f.log.String())
		}
	}
}

func TestGuestWatchdogEnvSetDMIEC2Arms(t *testing.T) {
	f, armed := armedWith(t, "req-1", map[string]string{dmiSysVendor: "Amazon EC2\n"})
	if !armed || f.powered[0] != 20*time.Minute {
		t.Fatalf("EC2 worker did not arm: powered=%v\n%s", f.powered, f.log.String())
	}
}

func TestGuestWatchdogEnvUnsetDMIEC2NeverArms(t *testing.T) {
	consulted := false
	f := newFakeGuest()
	cfg := f.config("")
	cfg.Machine = func(id string) error {
		consulted = true
		return EC2MachineFact(dmiFiles(map[string]string{dmiSysVendor: "Amazon EC2\n"}))(id)
	}
	if err := runWatchdog(t, cfg); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(f.powered) != 0 || consulted || strings.Contains(f.log.String(), "refusing") {
		t.Fatalf("non-worker EC2 host armed or logged: powered=%v consulted=%v\n%s", f.powered, consulted, f.log.String())
	}
}
