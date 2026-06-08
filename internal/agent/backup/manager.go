package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/scheduler"
)

// JobName is the registered scheduler name for the backup job. Stable
// constant so manual ledger queries and the admin UI agree on what
// row corresponds to a scheduled fire.
const JobName = "backup-folders"

// Manager glues admincore's saved BackupConfig to the in-process
// scheduler. The lifecycle is:
//
//   - main.go constructs one Manager with a process-lifetime
//     scheduler reference and the path resolver for the default
//     ledger (cache dir).
//   - At boot, Apply is called with the persisted FileConfig.Backup
//     so a scheduled job is registered before any HTTP serves.
//   - When the admin UI saves a new BackupConfig via admincore.
//     SetBackup, admincore calls Manager.Apply with the new config —
//     LIVE mutation, no restart needed.
//
// Manager owns the Worker (one per process) so RunOnce dedup is
// process-wide. Concurrent Apply + RunOnce is safe because both go
// through the worker's own inFlight mutex.
type Manager struct {
	sched      *scheduler.Scheduler
	defaultLog string // default ledger path when BackupConfig.LedgerPath is empty

	mu      sync.Mutex
	cfg     conf.BackupConfig
	worker  *Worker
	ledger  *Ledger
}

// NewManager constructs a Manager. scheduler must be non-nil;
// defaultLedger is the on-disk path the manager uses when the saved
// BackupConfig.LedgerPath is empty (typically
// <UserCacheDir>/outpost/backup.log).
func NewManager(sched *scheduler.Scheduler, defaultLedger string) *Manager {
	return &Manager{
		sched:      sched,
		defaultLog: defaultLedger,
	}
}

// Apply reconciles the scheduler against cfg: registers (or
// re-registers) the cron entry when cfg.Enabled && cfg.Schedule, or
// removes it otherwise. Updates the worker + ledger to point at the
// (possibly new) ledger path. Idempotent — safe to call on every
// save even when nothing changed.
func (m *Manager) Apply(cfg *conf.BackupConfig) error {
	if m == nil || m.sched == nil {
		return errors.New("backup: manager not constructed")
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	// Empty config = default-off. Remove any previously-registered
	// job and clear the worker so the admin UI's "Run now" returns
	// a clear "backup disabled" rather than firing into nothing.
	if cfg == nil {
		m.sched.Remove(JobName)
		m.cfg = conf.BackupConfig{}
		m.worker = nil
		m.ledger = nil
		return nil
	}

	ledgerPath := cfg.LedgerPath
	if ledgerPath == "" {
		ledgerPath = m.defaultLog
	}
	m.ledger = NewLedger(ledgerPath)
	m.worker = NewWorker(m.ledger)
	m.cfg = *cfg

	if !cfg.Enabled || cfg.Schedule == "" || len(cfg.Folders) == 0 {
		m.sched.Remove(JobName)
		return nil
	}
	// Capture folders by value so a later Apply doesn't race the
	// running job (it will re-register with the new folder list when
	// the next Apply lands).
	folders := append([]string(nil), cfg.Folders...)
	return m.sched.Register(JobName, cfg.Schedule, func(ctx context.Context) error {
		_, err := m.worker.RunOnce(ctx, folders)
		return err
	})
}

// RunNow triggers a manual fire against the currently-applied
// folders, regardless of Enabled. Returns the candidates produced
// (one per folder) so the admin UI can render the result inline.
// Returns an error when no config has been applied yet.
func (m *Manager) RunNow(ctx context.Context) ([]Candidate, error) {
	m.mu.Lock()
	worker := m.worker
	folders := append([]string(nil), m.cfg.Folders...)
	m.mu.Unlock()
	if worker == nil {
		return nil, errors.New("backup: no configuration applied yet")
	}
	if len(folders) == 0 {
		return nil, errors.New("backup: no folders configured")
	}
	return worker.RunOnce(ctx, folders)
}

// History returns the last `n` ledger entries (newest last). Empty
// when the manager has no ledger configured yet. Used by the admin
// UI's "Recent backups" panel and by future MCP/CLI surfaces.
func (m *Manager) History(n int) ([]Candidate, error) {
	m.mu.Lock()
	ledger := m.ledger
	m.mu.Unlock()
	if ledger == nil {
		return nil, nil
	}
	return ledger.Tail(n)
}

// DefaultLedgerPath returns "<cacheDir>/outpost/backup.log" or an
// empty string if UserCacheDir is unavailable. Used by main.go to
// build the default Manager.
func DefaultLedgerPath() string {
	cache, err := os.UserCacheDir()
	if err != nil || cache == "" {
		return ""
	}
	return filepath.Join(cache, "outpost", "backup.log")
}
