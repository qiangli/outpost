package upgrade

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPendingConfirmRoundTrip(t *testing.T) {
	p := PendingConfirmPath(t.TempDir())

	if got, err := ReadPendingConfirm(p); err != nil || got != nil {
		t.Fatalf("missing marker: got %+v, %v; want nil,nil", got, err)
	}
	pc := NewPendingConfirm("rel-9", "oldcommit", "newcommit", "/bin/outpost", "/bin/outpost.previous")
	if err := WritePendingConfirm(p, pc); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := ReadPendingConfirm(p)
	if err != nil || got == nil {
		t.Fatalf("read: %+v, %v", got, err)
	}
	if got.ReleaseID != "rel-9" || got.ToSHA != "newcomm" || got.FromSHA != "oldcomm" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if got.ConfirmDeadline.Before(got.SwappedAt) {
		t.Fatal("deadline should be after swap")
	}
	if err := ClearPendingConfirm(p); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if got, _ := ReadPendingConfirm(p); got != nil {
		t.Fatal("marker should be gone after clear")
	}
	// Clearing a missing marker is fine.
	if err := ClearPendingConfirm(p); err != nil {
		t.Fatalf("clear missing: %v", err)
	}
}

func TestArmConfirmCommitsOnDwell(t *testing.T) {
	old := confirmDwell
	confirmDwell = 10 * time.Millisecond
	t.Cleanup(func() { confirmDwell = old })

	dir := t.TempDir()
	p := PendingConfirmPath(dir)
	ledger := NewLedger(filepath.Join(dir, "upgrade.log"))
	_ = WritePendingConfirm(p, NewPendingConfirm("rel-1", "oldsha0", "newsha0", "/bin/x", "/bin/x.previous"))

	// Running commit == ToSHA → we're the new binary; survive the dwell.
	ArmConfirm(context.Background(), p, "newsha0", ledger)

	if got, _ := ReadPendingConfirm(p); got != nil {
		t.Fatal("marker should be cleared after a healthy dwell")
	}
	entries, _ := ledger.Tail(10)
	if len(entries) == 0 || entries[len(entries)-1].Step != "confirm_ok" {
		t.Fatalf("expected confirm_ok ledger entry, got %+v", entries)
	}
}

func TestArmConfirmDoesNotCommitOnEarlyCancel(t *testing.T) {
	old := confirmDwell
	confirmDwell = time.Hour // never reached
	t.Cleanup(func() { confirmDwell = old })

	dir := t.TempDir()
	p := PendingConfirmPath(dir)
	_ = WritePendingConfirm(p, NewPendingConfirm("rel-1", "oldsha0", "newsha0", "/bin/x", "/bin/x.previous"))

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // a crash/restart before the dwell elapses
	ArmConfirm(ctx, p, "newsha0", NewLedger(filepath.Join(dir, "u.log")))

	if got, _ := ReadPendingConfirm(p); got == nil {
		t.Fatal("marker must survive an early cancel so the supervisor can act")
	}
}

func TestArmConfirmClearsStaleMarkerAfterRevert(t *testing.T) {
	dir := t.TempDir()
	p := PendingConfirmPath(dir)
	// Marker is for an upgrade TO newsha0 FROM oldsha0; but we are running
	// oldsha0 (a revert already swapped us back) → marker is stale.
	_ = WritePendingConfirm(p, NewPendingConfirm("rel-1", "oldsha0", "newsha0", "/bin/x", "/bin/x.previous"))
	ArmConfirm(context.Background(), p, "oldsha0", NewLedger(filepath.Join(dir, "u.log")))
	if got, _ := ReadPendingConfirm(p); got != nil {
		t.Fatal("stale marker (we are the reverted-to binary) should be cleared")
	}
}

func TestRevertToPreviousGood(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "outpost")
	prev := binary + ".previous"
	if err := os.WriteFile(binary, []byte("NEW BAD BINARY"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A real probe-able fake as the rollback target.
	fake := fakeOutpostBinary(t, `{"commit":"deadbee1234","go_version":"go1.25"}`, 0)
	data, _ := os.ReadFile(fake)
	if err := os.WriteFile(prev, data, 0o755); err != nil {
		t.Fatal(err)
	}

	ledger := NewLedger(filepath.Join(dir, "upgrade.log"))
	build, err := RevertToPrevious(binary, prev, ledger, LedgerEntry{Step: "auto_rollback", ReleaseID: "rel-x"})
	if err != nil {
		t.Fatalf("RevertToPrevious: %v", err)
	}
	if build.Commit != "deadbee1234" {
		t.Fatalf("restored build commit = %q", build.Commit)
	}
	// The live binary must now be the previous (probe-able) one, not the bad one.
	got, _ := os.ReadFile(binary)
	if string(got) == "NEW BAD BINARY" {
		t.Fatal("revert did not replace the live binary")
	}
	if _, err := os.Stat(prev); !os.IsNotExist(err) {
		t.Fatal("previous should be consumed (renamed) by the revert")
	}
	entries, _ := ledger.Tail(10)
	if len(entries) == 0 || entries[len(entries)-1].Step != "auto_rollback" {
		t.Fatalf("expected auto_rollback ledger entry, got %+v", entries)
	}
}

func TestRevertToPreviousMissing(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "outpost")
	_ = os.WriteFile(binary, []byte("live"), 0o755)
	_, err := RevertToPrevious(binary, binary+".previous", nil, LedgerEntry{Step: "rollback"})
	if err != ErrNoPrevious {
		t.Fatalf("missing previous: err = %v, want ErrNoPrevious", err)
	}
}

func TestRevertToPreviousRefusesCorrupt(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "outpost")
	prev := binary + ".previous"
	_ = os.WriteFile(binary, []byte("GOOD LIVE BINARY"), 0o755)
	// A .previous that fails to probe (exit 1) — a truncated/wrong-platform file.
	bad := fakeOutpostBinary(t, `{"commit":"x"}`, 1)
	data, _ := os.ReadFile(bad)
	_ = os.WriteFile(prev, data, 0o755)

	if _, err := RevertToPrevious(binary, prev, nil, LedgerEntry{Step: "auto_rollback"}); err == nil {
		t.Fatal("expected RevertToPrevious to refuse a corrupt .previous")
	}
	// The live binary must be untouched — never swap in a broken rollback target.
	if got, _ := os.ReadFile(binary); string(got) != "GOOD LIVE BINARY" {
		t.Fatal("live binary was clobbered by a refused revert")
	}
}

func TestApplyRefusesQuarantinedRelease(t *testing.T) {
	dir := t.TempDir()
	qp := QuarantinePath(dir)
	q := NewQuarantine(qp)
	_ = q.Add(QuarantineEntry{ReleaseID: "rel-bad", Reason: "auto-reverted"})

	w, err := NewWorker(Options{
		State:          func() StateSnapshot { return StateSnapshot{CurrentCommit: "aaaaaaa", BinaryPath: "/bin/outpost"} },
		Restart:        func() {},
		QuarantinePath: qp,
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	res := w.Apply(context.Background(), Envelope{
		ReleaseID: "rel-bad",
		URL:       "https://example.com/outpost",
		SHA256:    "abc",
		Commit:    "bbbbbbb",
	})
	if res.Status != StatusQuarantined {
		t.Fatalf("Apply status = %q, want quarantined", res.Status)
	}
	// A non-quarantined release is not blocked by the guard (it proceeds to
	// the normal path; here it will accept + stage, which we don't drive).
	res2 := w.Apply(context.Background(), Envelope{
		ReleaseID: "rel-ok", URL: "https://example.com/o", SHA256: "abc", Commit: "bbbbbbb",
	})
	if res2.Status == StatusQuarantined {
		t.Fatal("non-quarantined release should not be blocked")
	}
}
