package upgrade

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/outpost/internal/agent"
)

func TestRepairDetectionAndApproval(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "heals.jsonl")
			r := RepairLedger{Path: path}
			cur := agent.BuildInfo{Version: "v1.0.0"}
			env := Envelope{ReleaseID: "v1.1.0", URL: "https://example.invalid/release", SHA256: strings.Repeat("a", 64), Commit: "abcdef1"}
			for i := 0; i < 2; i++ {
				if err := r.Observe(cur, env, false, "", false); err != nil {
					t.Fatal(err)
				}
			}
			rows, err := agent.ReadHealRecords(path)
			if err != nil || len(rows) != 1 {
				t.Fatalf("rows=%v err=%v", rows, err)
			}
			if rows[0].Result != "outdated" {
				t.Fatal(rows[0])
			}
			for i := 0; i < 3; i++ {
				if err := r.Observe(cur, env, false, "declined by guard", false); err != nil {
					t.Fatal(err)
				}
			}
			rows, _ = agent.ReadHealRecords(path)
			if len(rows) != 1 || rows[0].Result != "stuck" {
				t.Fatal(rows)
			}
			calls := 0
			err = r.Approve(context.Background(), rows[0].ID, func(_ context.Context, got Envelope) error {
				calls++
				if got.URL != env.URL || got.SHA256 != env.SHA256 {
					t.Fatal(got)
				}
				if fail {
					return errors.New("fake swap failed")
				}
				return nil
			})
			if (err != nil) != fail || calls != 1 {
				t.Fatalf("err=%v calls=%d", err, calls)
			}
			rows, _ = agent.ReadHealRecords(path)
			want := "healed"
			if fail {
				want = "failed"
			}
			if rows[0].Result != want {
				t.Fatal(rows)
			}
		})
	}
}
func TestRepairCurrentAndAuto(t *testing.T) {
	r := RepairLedger{Path: filepath.Join(t.TempDir(), "heals.jsonl")}
	env := Envelope{ReleaseID: "v1.1.0", URL: "https://example.invalid/release", SHA256: strings.Repeat("a", 64), Commit: "abcdef1"}
	if err := r.Observe(agent.BuildInfo{Version: env.ReleaseID}, env, false, "", false); err != nil {
		t.Fatal(err)
	}
	rows, _ := agent.ReadHealRecords(r.Path)
	if len(rows) != 0 {
		t.Fatal(rows)
	}
	cur := agent.BuildInfo{Version: "v1.0.0"}
	if err := r.Observe(cur, env, true, "", false); err != nil {
		t.Fatal(err)
	}
	if err := r.Observe(cur, env, true, "", true); err != nil {
		t.Fatal(err)
	}
	rows, _ = agent.ReadHealRecords(r.Path)
	if len(rows) != 1 || rows[0].Result != "healed" {
		t.Fatal(rows)
	}
}
func TestRepairStuckByAge(t *testing.T) {
	now := time.Now()
	r := RepairLedger{Path: filepath.Join(t.TempDir(), "heals.jsonl"), Now: func() time.Time { return now }}
	env := Envelope{ReleaseID: "v1.1.0", URL: "https://example.invalid/release", SHA256: strings.Repeat("a", 64), Commit: "abcdef1"}
	cur := agent.BuildInfo{Version: "v1.0.0"}
	_ = r.Observe(cur, env, true, "declined", false)
	now = now.Add(7 * time.Hour)
	_ = r.Observe(cur, env, true, "declined", false)
	rows, _ := agent.ReadHealRecords(r.Path)
	if len(rows) != 1 || rows[0].Result != "stuck" {
		t.Fatal(rows)
	}
}

func TestRepairFailedPairLeavesOriginalFiles(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "outpost")
	candidate := filepath.Join(dir, "candidate")
	if err := os.WriteFile(binary, []byte("old binary"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(candidate, []byte("new binary"), 0700); err != nil {
		t.Fatal(err)
	}
	r := RepairLedger{Path: filepath.Join(dir, "heals.jsonl")}
	env := Envelope{ReleaseID: "v1.1.0", URL: "https://example.invalid/release", SHA256: strings.Repeat("a", 64)}
	if err := r.Observe(agent.BuildInfo{Version: "v1.0.0"}, env, false, "", false); err != nil {
		t.Fatal(err)
	}
	rows, _ := agent.ReadHealRecords(r.Path)
	err := r.Approve(context.Background(), rows[0].ID, func(context.Context, Envelope) error {
		return applyPair(binary, candidate, map[string]string{productName("bashy"): filepath.Join(dir, "missing")}, nil)
	})
	if err == nil {
		t.Fatal("expected failed pair")
	}
	b, err := os.ReadFile(binary)
	if err != nil || string(b) != "old binary" {
		t.Fatalf("live file changed: %q %v", b, err)
	}
	rows, _ = agent.ReadHealRecords(r.Path)
	if rows[0].Result != "failed" {
		t.Fatal(rows)
	}
}

func TestRepairBashyFailureAndDedup(t *testing.T) {
	r := RepairLedger{Path: filepath.Join(t.TempDir(), "heals.jsonl"), Component: "bashy"}
	env := Envelope{ReleaseID: "v1.1.0"}
	for i := 0; i < 2; i++ {
		if err := r.RecordMismatch("v1.0.0", env, errors.New("resolve failed")); err != nil {
			t.Fatal(err)
		}
	}
	rows, _ := agent.ReadHealRecords(r.Path)
	if len(rows) != 1 || rows[0].App != "bashy" || rows[0].Result != "stuck" {
		t.Fatal(rows)
	}
	called := false
	if err := r.Approve(context.Background(), rows[0].ID, func(context.Context, Envelope) error { called = true; return nil }); err == nil || called {
		t.Fatal("unverified repair executed")
	}
}

func TestWorkerRepairApprovalAndDeclines(t *testing.T) {
	h := newHarness(t)
	h.setState(func(s *StateSnapshot) { s.CurrentVersion = "v1.0.0" })
	r := &RepairLedger{Path: filepath.Join(h.dir, "heals.jsonl")}
	h.worker.repairs = r
	h.worker.automatic = func() bool { return false }
	env := Envelope{ReleaseID: "v1.1.0", URL: "https://example.invalid/release", SHA256: strings.Repeat("a", 64), Commit: "abcdef1", MinFrom: "fffffff"}
	for i := 0; i < 2; i++ {
		result := h.worker.Apply(context.Background(), env)
		if result.Status != StatusPendingManual {
			t.Fatal(result)
		}
	}
	rows, _ := agent.ReadHealRecords(r.Path)
	if len(rows) != 1 || rows[0].Result != "outdated" {
		t.Fatal(rows)
	}
	h.worker.automatic = func() bool { return true }
	for i := 0; i < 3; i++ {
		result := h.worker.Apply(context.Background(), env)
		if result.Status != StatusMinFrom {
			t.Fatal(result)
		}
	}
	rows, _ = agent.ReadHealRecords(r.Path)
	if len(rows) != 1 || rows[0].Result != "stuck" {
		t.Fatal(rows)
	}
	evidence, err := repairEvidence(rows[0])
	if err != nil || evidence.Attempts != 3 {
		t.Fatalf("evidence=%+v err=%v", evidence, err)
	}
}

func TestRepairApprovalRejectsDuplicate(t *testing.T) {
	r := RepairLedger{Path: filepath.Join(t.TempDir(), "heals.jsonl")}
	env := Envelope{ReleaseID: "v1.1.0", URL: "https://example.invalid/release", SHA256: strings.Repeat("a", 64)}
	if err := r.Observe(agent.BuildInfo{Version: "v1.0.0"}, env, false, "", false); err != nil {
		t.Fatal(err)
	}
	rows, _ := agent.ReadHealRecords(r.Path)
	if err := r.Approve(context.Background(), rows[0].ID, func(context.Context, Envelope) error {
		returnCheck := r.Approve(context.Background(), rows[0].ID, func(context.Context, Envelope) error { t.Fatal("duplicate apply"); return nil })
		if returnCheck == nil {
			t.Fatal("duplicate approval accepted")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
