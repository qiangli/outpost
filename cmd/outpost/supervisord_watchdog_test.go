package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/qiangli/outpost/internal/agent/supervisor"
	"github.com/qiangli/outpost/internal/agent/upgrade"
)

// fakeProbeBinary builds a tiny program that answers `version --json` with
// the given body and exit code — enough for upgrade.Probe to accept/reject
// it as a rollback target. Otherwise it records startup in its first argument
// and hangs without confirming, so a supervisor must stop it at the deadline.
func fakeProbeBinary(t *testing.T, jsonBody string, exit int) []byte {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "fake.go")
	body := "package main\nimport (\"fmt\";\"os\";\"time\")\nfunc main(){\nif len(os.Args)>=3 && os.Args[1]==\"version\" && os.Args[2]==\"--json\"{\nfmt.Print(`" + jsonBody + "`)\nos.Exit(" + strconv.Itoa(exit) + ")\n}\nif len(os.Args)>1 { os.WriteFile(os.Args[1], []byte(`" + jsonBody + "`), 0600) }; for { time.Sleep(time.Hour) }\n}\n"
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "fake")
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	c := exec.Command("go", "build", "-o", out, src)
	c.Stdout, c.Stderr = os.Stderr, os.Stderr
	if err := c.Run(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// armMarker writes a pending-confirm marker for a just-upgraded binary,
// laying down a live "new" binary and an optional probe-able .previous.
func armMarker(t *testing.T, dir string, prevBytes []byte) (binary, confirmPath string) {
	t.Helper()
	binary = filepath.Join(dir, "outpost")
	if err := os.WriteFile(binary, []byte("NEW BAD BINARY"), 0o755); err != nil {
		t.Fatal(err)
	}
	prev := binary + ".previous"
	if prevBytes != nil {
		if err := os.WriteFile(prev, prevBytes, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	confirmPath = upgrade.PendingConfirmPath(dir)
	if err := upgrade.WritePendingConfirm(confirmPath,
		upgrade.NewPendingConfirm("rel-bad", "oldsha0", "newsha0", binary, prev)); err != nil {
		t.Fatal(err)
	}
	return binary, confirmPath
}

func TestWatchdogAlwaysRevertsUnconfirmed(t *testing.T) {
	dir := t.TempDir()
	binary, confirmPath := armMarker(t, dir, fakeProbeBinary(t, `{"commit":"deadbee1234","go_version":"go1.25"}`, 0))
	ledger := upgrade.NewLedger(filepath.Join(dir, "upgrade.log"))
	q := upgrade.NewQuarantine(upgrade.QuarantinePath(dir))

	// No flag, no opt-in: the watchdog ALWAYS reverts a just-upgraded
	// binary that never confirms healthy.
	hook := newWatchdogHook(confirmPath, ledger, q)

	old := upgrade.MaxUnconfirmedBoots
	upgrade.MaxUnconfirmedBoots = 1
	t.Cleanup(func() { upgrade.MaxUnconfirmedBoots = old })

	if err := hook(); err != nil {
		t.Fatalf("hook: %v", err)
	}
	if got, _ := os.ReadFile(binary); string(got) == "NEW BAD BINARY" {
		t.Fatal("watchdog must revert an unconfirmed binary with no flag set")
	}
	if !q.Has("rel-bad") {
		t.Fatal("revert must quarantine the bad release")
	}
	if pc, _ := upgrade.ReadPendingConfirm(confirmPath); pc != nil {
		t.Fatal("marker should be cleared after a successful revert")
	}
	entries, _ := ledger.Tail(10)
	if len(entries) == 0 || entries[len(entries)-1].Step != "auto_rollback" {
		t.Fatalf("expected auto_rollback ledger entry, got %+v", entries)
	}
}

func TestWatchdogArmedRevertsOnCrashLoop(t *testing.T) {
	dir := t.TempDir()
	prev := fakeProbeBinary(t, `{"commit":"deadbee1234","go_version":"go1.25"}`, 0)
	binary, confirmPath := armMarker(t, dir, prev)
	ledger := upgrade.NewLedger(filepath.Join(dir, "upgrade.log"))
	q := upgrade.NewQuarantine(upgrade.QuarantinePath(dir))

	old := upgrade.MaxUnconfirmedBoots
	upgrade.MaxUnconfirmedBoots = 1 // one unconfirmed boot = crash loop
	t.Cleanup(func() { upgrade.MaxUnconfirmedBoots = old })

	hook := newWatchdogHook(confirmPath, ledger, q)
	if err := hook(); err != nil {
		t.Fatalf("hook: %v", err)
	}

	if got, _ := os.ReadFile(binary); string(got) == "NEW BAD BINARY" {
		t.Fatal("armed watchdog should have reverted the bad binary")
	}
	if !q.Has("rel-bad") {
		t.Fatal("armed revert should quarantine the bad release")
	}
	if pc, _ := upgrade.ReadPendingConfirm(confirmPath); pc != nil {
		t.Fatal("marker should be cleared after a successful revert")
	}
	entries, _ := ledger.Tail(10)
	if len(entries) == 0 || entries[len(entries)-1].Step != "auto_rollback" {
		t.Fatalf("expected auto_rollback ledger entry, got %+v", entries)
	}
}

func TestWatchdogArmedKeepsBinaryWhenPreviousCorrupt(t *testing.T) {
	dir := t.TempDir()
	// .previous that fails to probe (exit 1) — a double-brick.
	binary, confirmPath := armMarker(t, dir, fakeProbeBinary(t, `{"commit":"x"}`, 1))
	ledger := upgrade.NewLedger(filepath.Join(dir, "upgrade.log"))
	q := upgrade.NewQuarantine(upgrade.QuarantinePath(dir))

	old := upgrade.MaxUnconfirmedBoots
	upgrade.MaxUnconfirmedBoots = 1
	t.Cleanup(func() { upgrade.MaxUnconfirmedBoots = old })

	hook := newWatchdogHook(confirmPath, ledger, q)
	if err := hook(); err != nil {
		t.Fatalf("hook: %v", err)
	}
	// Refused to swap a broken .previous → live binary untouched, marker kept.
	if got, _ := os.ReadFile(binary); string(got) != "NEW BAD BINARY" {
		t.Fatal("must NOT swap in a corrupt .previous")
	}
	if q.Has("rel-bad") {
		t.Fatal("a failed revert must not quarantine")
	}
	if pc, _ := upgrade.ReadPendingConfirm(confirmPath); pc == nil {
		t.Fatal("marker should be kept after a failed revert")
	}
	entries, _ := ledger.Tail(10)
	if len(entries) == 0 || entries[len(entries)-1].Step != "auto_rollback_failed" {
		t.Fatalf("expected auto_rollback_failed ledger entry, got %+v", entries)
	}
}

func TestWatchdogNoMarkerIsNoop(t *testing.T) {
	dir := t.TempDir()
	hook := newWatchdogHook(upgrade.PendingConfirmPath(dir),
		upgrade.NewLedger(filepath.Join(dir, "upgrade.log")),
		upgrade.NewQuarantine(upgrade.QuarantinePath(dir)))
	if err := hook(); err != nil {
		t.Fatalf("no-marker hook should be a clean no-op, got %v", err)
	}
}

func TestWatchdogDeadlineObservesWithoutMutating(t *testing.T) {
	dir := t.TempDir()
	_, path := armMarker(t, dir, nil)
	check := watchdogDeadline(path)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if check() {
		t.Fatal("deadline has not passed")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("polling changed the marker or boot count")
	}
	pc, err := upgrade.ReadPendingConfirm(path)
	if err != nil {
		t.Fatal(err)
	}
	pc.ConfirmDeadline = time.Now().Add(-time.Second)
	if err := upgrade.WritePendingConfirm(path, *pc); err != nil {
		t.Fatal(err)
	}
	if !check() {
		t.Fatal("unconfirmed deadline must trigger restart")
	}
	if err := upgrade.ClearPendingConfirm(path); err != nil {
		t.Fatal(err)
	}
	if check() {
		t.Fatal("confirmed upgrade must not trigger restart")
	}
}

func TestWatchdogRevertsHangingDaemon(t *testing.T) {
	dir := t.TempDir()
	const previousVersion = `{"commit":"deadbee1234","go_version":"go1.25"}`
	previous := fakeProbeBinary(t, previousVersion, 0)
	candidate := fakeProbeBinary(t, `{"commit":"badbeef1234","go_version":"go1.25"}`, 0)
	binary, confirmPath := armMarker(t, dir, previous)
	if err := os.WriteFile(binary, candidate, 0755); err != nil {
		t.Fatal(err)
	}
	started := filepath.Join(dir, "started")
	p := &supervisor.Program{
		Name: "daemon", Path: binary, Args: []string{started},
		MinBackoff: 10 * time.Millisecond, MaxBackoff: 20 * time.Millisecond,
		RestartCheckInterval: 20 * time.Millisecond,
	}
	attachWatchdog([]*supervisor.Program{p}, dir)
	m := supervisor.New(p)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(15 * time.Second):
			t.Error("supervisor failed to stop")
		}
	})
	wait := func(what string, condition func() bool) {
		t.Helper()
		limit := time.Now().Add(5 * time.Second)
		for !condition() {
			if time.Now().After(limit) {
				t.Fatalf("timed out waiting for %s; status: %+v", what, m.Snapshot())
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	wait("candidate startup", func() bool { _, err := os.Stat(started); return err == nil })
	initial := m.Snapshot()[0]
	if initial.Starts != 1 || !initial.Running {
		t.Fatalf("candidate should be hanging on its first boot: %+v", initial)
	}
	pc, err := upgrade.ReadPendingConfirm(confirmPath)
	if err != nil || pc == nil {
		t.Fatalf("pending marker: %v", err)
	}
	pc.ConfirmDeadline = time.Now().Add(150 * time.Millisecond)
	if err := upgrade.WritePendingConfirm(confirmPath, *pc); err != nil {
		t.Fatal(err)
	}
	wait("reverted daemon startup", func() bool {
		data, _ := os.ReadFile(started)
		return string(data) == previousVersion
	})
	status := m.Snapshot()[0]
	if status.Starts != 2 || !status.Running || status.PID == initial.PID {
		t.Fatalf("expected one watchdog restart: %+v", status)
	}
	if time.Now().Before(pc.ConfirmDeadline) {
		t.Fatal("reverted before the deadline")
	}
	if got, err := os.ReadFile(binary); err != nil || !bytes.Equal(got, previous) {
		t.Fatalf("previous binary was not restored: %v", err)
	}
	if pc, err := upgrade.ReadPendingConfirm(confirmPath); err != nil || pc != nil {
		t.Fatalf("marker not cleared: %+v, %v", pc, err)
	}
	if !upgrade.NewQuarantine(upgrade.QuarantinePath(dir)).Has("rel-bad") {
		t.Fatal("failed release not quarantined")
	}
	entries, err := upgrade.NewLedger(filepath.Join(dir, "upgrade.log")).Tail(10)
	if err != nil || len(entries) == 0 || entries[len(entries)-1].Step != "auto_rollback" {
		t.Fatalf("missing auto_rollback ledger entry: %+v, %v", entries, err)
	}
}

// TestSupervisordWiresWatchdogPreStart pins the second half of the boot
// chain: superviseOnce must attach the auto-rollback watchdog to the
// daemon program before the supervisor runs it, on every platform
// (including a Windows host booted via the S4U task, which execs
// `outpost supervisord` and nothing else).
func TestSupervisordWiresWatchdogPreStart(t *testing.T) {
	dir := t.TempDir()
	programs := supervisordPrograms("/opt/outpost", "/opt/bashy", "", "")
	attachWatchdog(programs, dir)
	if len(programs) != 2 {
		t.Fatalf("supervisordPrograms returned %d programs, want 2", len(programs))
	}
	if programs[0].PreStart == nil {
		t.Fatal("daemon program has no watchdog PreStart — an unconfirmed upgrade would never revert")
	}
	if programs[0].RestartWhen == nil {
		t.Fatal("daemon program has no deadline check while running")
	}
	if programs[1].PreStart != nil || programs[1].RestartWhen != nil {
		t.Fatal("bashy monitor program must not carry the outpost watchdog hook")
	}
}
