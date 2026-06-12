package admincore

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/outpost/internal/agent/backup"
	"github.com/qiangli/outpost/internal/agent/conf"
)

// stubApplier records what was applied so tests can assert the
// admincore→manager handoff without spinning up a real scheduler.
type stubApplier struct {
	applied   *conf.BackupConfig
	runCalled bool
}

func (s *stubApplier) Apply(cfg *conf.BackupConfig) error {
	if cfg == nil {
		s.applied = nil
	} else {
		c := *cfg
		s.applied = &c
	}
	return nil
}

func (s *stubApplier) RunNow(ctx context.Context) ([]backup.Candidate, error) {
	s.runCalled = true
	return []backup.Candidate{{Folder: "/x", SHA256: "deadbeef"}}, nil
}

func (s *stubApplier) History(n int) ([]backup.Candidate, error) {
	return []backup.Candidate{{Folder: "/x"}}, nil
}

func newServerForBackup(t *testing.T, applier *stubApplier) *Server {
	t.Helper()
	dir := t.TempDir()
	srv, err := New(Deps{
		ConfigPath: filepath.Join(dir, "agent.json"),
		Backup:     applier,
	})
	if err != nil {
		t.Fatalf("admincore.New: %v", err)
	}
	return srv
}

func TestGetBackup_EmptyByDefault(t *testing.T) {
	srv := newServerForBackup(t, &stubApplier{})
	got, err := srv.GetBackup()
	if err != nil {
		t.Fatalf("GetBackup: %v", err)
	}
	if got.Enabled || got.Schedule != "" || len(got.Folders) != 0 {
		t.Errorf("expected zero config, got %+v", got)
	}
}

func TestSetBackup_RoundTrip(t *testing.T) {
	applier := &stubApplier{}
	srv := newServerForBackup(t, applier)
	p := BackupParams{
		Enabled:  true,
		Schedule: "0 2 * * *",
		Folders:  []string{"./relative-path", "/abs/path"},
	}
	saved, err := srv.SetBackup(p)
	if err != nil {
		t.Fatalf("SetBackup: %v", err)
	}
	if !saved.Enabled {
		t.Error("Enabled should round-trip")
	}
	if saved.Schedule != "0 2 * * *" {
		t.Errorf("Schedule round-trip mismatch: %q", saved.Schedule)
	}
	if len(saved.Folders) != 2 {
		t.Fatalf("expected 2 folders, got %v", saved.Folders)
	}
	// Folders must be absolute-path normalised.
	for _, f := range saved.Folders {
		if !filepath.IsAbs(f) {
			t.Errorf("folder %q should be absolute", f)
		}
	}
	// Applier should have been called with the normalised config.
	if applier.applied == nil || applier.applied.Schedule != "0 2 * * *" {
		t.Errorf("applier should have seen the saved config, got %+v", applier.applied)
	}
	// Reading back through GetBackup should match.
	got, err := srv.GetBackup()
	if err != nil {
		t.Fatalf("GetBackup: %v", err)
	}
	if got.Schedule != saved.Schedule || len(got.Folders) != len(saved.Folders) {
		t.Errorf("GetBackup after Set: got %+v, want %+v", got, saved)
	}
}

func TestSetBackup_TrimsBlankFolders(t *testing.T) {
	srv := newServerForBackup(t, &stubApplier{})
	saved, err := srv.SetBackup(BackupParams{
		Enabled: true, Schedule: "@daily",
		Folders: []string{"  ", "/tmp/a", "", "/tmp/b", "   "},
	})
	if err != nil {
		t.Fatalf("SetBackup: %v", err)
	}
	if len(saved.Folders) != 2 {
		t.Errorf("blank lines should be dropped, got %v", saved.Folders)
	}
}

func TestSetBackup_RejectsBadSchedule(t *testing.T) {
	srv := newServerForBackup(t, &stubApplier{})
	_, err := srv.SetBackup(BackupParams{
		Enabled: true, Schedule: "not a cron expr",
		Folders: []string{"/tmp/x"},
	})
	if err == nil {
		t.Fatal("expected error for bad schedule")
	}
	if !strings.Contains(err.Error(), "invalid schedule") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSetBackup_EnabledRequiresScheduleAndFolders(t *testing.T) {
	srv := newServerForBackup(t, &stubApplier{})
	if _, err := srv.SetBackup(BackupParams{Enabled: true}); err == nil {
		t.Error("Enabled without schedule should error")
	}
	if _, err := srv.SetBackup(BackupParams{Enabled: true, Schedule: "@daily"}); err == nil {
		t.Error("Enabled without folders should error")
	}
}

func TestSetBackup_DisabledAllowsEmptySchedule(t *testing.T) {
	srv := newServerForBackup(t, &stubApplier{})
	// Operator may want to save a disabled draft with just folders.
	saved, err := srv.SetBackup(BackupParams{
		Enabled: false,
		Folders: []string{"/tmp/x"},
	})
	if err != nil {
		t.Fatalf("disabled config should not require schedule: %v", err)
	}
	if saved.Enabled {
		t.Error("Enabled should round-trip false")
	}
}

func TestRunBackupNow_DelegatesToApplier(t *testing.T) {
	applier := &stubApplier{}
	srv := newServerForBackup(t, applier)
	out, err := srv.RunBackupNow(context.Background())
	if err != nil {
		t.Fatalf("RunBackupNow: %v", err)
	}
	if !applier.runCalled {
		t.Error("Applier.RunNow should have been called")
	}
	if len(out) != 1 || out[0].SHA256 != "deadbeef" {
		t.Errorf("expected stub candidate, got %+v", out)
	}
}

func TestRunBackupNow_UnwiredManagerErrors(t *testing.T) {
	srv := newServerForBackup(t, nil)
	srv.deps.Backup = nil // simulate "no manager wired"
	_, err := srv.RunBackupNow(context.Background())
	if err == nil {
		t.Error("expected error when manager not wired")
	}
}

func TestBackupHistory_DelegatesToApplier(t *testing.T) {
	applier := &stubApplier{}
	srv := newServerForBackup(t, applier)
	out, err := srv.BackupHistory(10)
	if err != nil {
		t.Fatalf("BackupHistory: %v", err)
	}
	if len(out) != 1 {
		t.Errorf("expected 1 candidate from stub, got %v", out)
	}
}
