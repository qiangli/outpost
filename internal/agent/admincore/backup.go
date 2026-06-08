package admincore

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/robfig/cron/v3"

	"github.com/qiangli/outpost/internal/agent/backup"
	"github.com/qiangli/outpost/internal/agent/conf"
)

// BackupApplier is what admincore needs from main.go's backup.Manager
// without taking on the package import in Deps (admincore stays
// protocol-agnostic; the backup package is implementation-specific).
type BackupApplier interface {
	Apply(cfg *conf.BackupConfig) error
	RunNow(ctx context.Context) ([]backup.Candidate, error)
	History(n int) ([]backup.Candidate, error)
}

// AttachBackup injects the live backup.Manager after admincore
// construction. Same setter pattern as AttachUpgrade — the manager
// needs the scheduler which is built alongside the errgroup, so it
// can't be passed through the initial Deps. Safe to call once at
// startup, no concurrent readers yet.
func (s *Server) AttachBackup(applier BackupApplier) {
	s.deps.Backup = applier
}

// GetBackup returns the persisted backup config — never nil. Empty
// fields mean "feature not configured yet" which the UI renders as a
// blank form.
func (s *Server) GetBackup() (conf.BackupConfig, error) {
	fc, err := s.loadConfig()
	if err != nil {
		return conf.BackupConfig{}, err
	}
	if fc.Backup == nil {
		return conf.BackupConfig{}, nil
	}
	return *fc.Backup, nil
}

// BackupParams is the wire shape the admin UI POSTs. Folder paths are
// trimmed and absolute-path-normalised before persisting; empty lines
// are dropped (the UI accepts a textarea so blank lines are common).
type BackupParams struct {
	Enabled    bool     `json:"enabled"`
	Schedule   string   `json:"schedule"`
	Folders    []string `json:"folders"`
	LedgerPath string   `json:"ledger_path,omitempty"`
}

// SetBackup validates the params, persists them into FileConfig, and
// re-applies the live scheduler entry via the Applier. LIVE mutation —
// no restart needed (the scheduler's Register replaces any prior
// entry for the same name).
//
// Validation:
//   - Schedule, when non-empty, must parse under cron/v3's standard
//     5-field parser plus descriptors.
//   - Folders are required when Enabled (no point in scheduling
//     against nothing). Each path is checked for absoluteness only;
//     existence is NOT enforced because the cooperating app may not
//     have written its first artifact yet.
func (s *Server) SetBackup(p BackupParams) (conf.BackupConfig, error) {
	cfg := conf.BackupConfig{
		Enabled:    p.Enabled,
		Schedule:   strings.TrimSpace(p.Schedule),
		LedgerPath: strings.TrimSpace(p.LedgerPath),
	}
	for _, raw := range p.Folders {
		clean := strings.TrimSpace(raw)
		if clean == "" {
			continue
		}
		abs, err := filepath.Abs(clean)
		if err != nil {
			return conf.BackupConfig{}, badRequest("invalid folder %q: %s", raw, err.Error())
		}
		cfg.Folders = append(cfg.Folders, abs)
	}
	if cfg.Schedule != "" {
		parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
		if _, err := parser.Parse(cfg.Schedule); err != nil {
			return conf.BackupConfig{}, badRequest("invalid schedule %q: %s", cfg.Schedule, err.Error())
		}
	}
	if cfg.Enabled {
		if cfg.Schedule == "" {
			return conf.BackupConfig{}, badRequest("schedule is required when backup is enabled")
		}
		if len(cfg.Folders) == 0 {
			return conf.BackupConfig{}, badRequest("at least one folder is required when backup is enabled")
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadConfig()
	if err != nil {
		return conf.BackupConfig{}, err
	}
	fc.Backup = &cfg
	if err := conf.SaveFile(s.deps.ConfigPath, fc); err != nil {
		return conf.BackupConfig{}, internalErr("%s", err.Error())
	}

	if s.deps.Backup != nil {
		if err := s.deps.Backup.Apply(&cfg); err != nil {
			// Persistence succeeded; live re-apply failed. The next
			// restart will pick up the persisted config, so the error
			// is informational — return it so the UI can surface a
			// warning but don't undo the save.
			return cfg, internalErr("config saved but live apply failed: %s", err.Error())
		}
	}
	return cfg, nil
}

// RunBackupNow triggers an immediate fire against the currently-
// applied folders, regardless of Enabled. Returns the candidates so
// the admin UI can render the result inline ("3 folders scanned;
// 1 new file picked, 2 skipped").
func (s *Server) RunBackupNow(ctx context.Context) ([]backup.Candidate, error) {
	if s.deps.Backup == nil {
		return nil, unavailable("backup manager not wired")
	}
	out, err := s.deps.Backup.RunNow(ctx)
	if err != nil {
		return nil, badRequest("%s", err.Error())
	}
	return out, nil
}

// BackupHistory returns the last `n` ledger entries (newest last).
// n<=0 returns all. Used by the admin UI's "Recent backups" panel.
func (s *Server) BackupHistory(n int) ([]backup.Candidate, error) {
	if s.deps.Backup == nil {
		return nil, nil
	}
	return s.deps.Backup.History(n)
}
