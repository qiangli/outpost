package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"github.com/qiangli/outpost/internal/agent/upgrade"
)

// fakeProbeBinary builds a tiny program that answers `version --json` with
// the given body and exit code — enough for upgrade.Probe to accept/reject
// it as a rollback target.
func fakeProbeBinary(t *testing.T, jsonBody string, exit int) []byte {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "fake.go")
	body := "package main\nimport (\"fmt\";\"os\")\nfunc main(){\nif len(os.Args)>=3 && os.Args[1]==\"version\" && os.Args[2]==\"--json\"{\nfmt.Print(`" + jsonBody + "`)\nos.Exit(" + strconv.Itoa(exit) + ")\n}\nos.Exit(2)\n}\n"
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

func TestWatchdogObserveModeNeverReverts(t *testing.T) {
	dir := t.TempDir()
	binary, confirmPath := armMarker(t, dir, fakeProbeBinary(t, `{"commit":"deadbee1234","go_version":"go1.25"}`, 0))
	ledger := upgrade.NewLedger(filepath.Join(dir, "upgrade.log"))
	q := upgrade.NewQuarantine(upgrade.QuarantinePath(dir))

	hook := newWatchdogHook(confirmPath, ledger, q, func() bool { return false }) // observe

	old := upgrade.MaxUnconfirmedBoots
	upgrade.MaxUnconfirmedBoots = 1
	t.Cleanup(func() { upgrade.MaxUnconfirmedBoots = old })

	// Even past the crash-loop threshold, observe mode must not revert.
	for i := 0; i < 3; i++ {
		if err := hook(); err != nil {
			t.Fatalf("hook: %v", err)
		}
	}
	if got, _ := os.ReadFile(binary); string(got) != "NEW BAD BINARY" {
		t.Fatal("observe mode must NOT swap the binary")
	}
	if q.Has("rel-bad") {
		t.Fatal("observe mode must NOT quarantine")
	}
	if pc, _ := upgrade.ReadPendingConfirm(confirmPath); pc == nil || pc.BootCount < 3 {
		t.Fatalf("observe mode should keep the marker and count boots, got %+v", pc)
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

	hook := newWatchdogHook(confirmPath, ledger, q, func() bool { return true }) // armed
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

	hook := newWatchdogHook(confirmPath, ledger, q, func() bool { return true })
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
		upgrade.NewQuarantine(upgrade.QuarantinePath(dir)),
		func() bool { return true })
	if err := hook(); err != nil {
		t.Fatalf("no-marker hook should be a clean no-op, got %v", err)
	}
}
