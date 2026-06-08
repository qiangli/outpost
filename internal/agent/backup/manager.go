package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/scheduler"
)

// filepathBase returns the basename of folder, trimming trailing
// slashes first so "/foo/bar/" returns "bar" not "". Pulled out as
// a named helper because the worker / pusher / manager all reach for
// it inline.
func filepathBase(folder string) string {
	folder = strings.TrimRight(folder, "/")
	return filepath.Base(folder)
}

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
//
// When a Pusher is attached, the manager pushes every fresh
// (non-skipped, non-errored) candidate to cloudbox after the worker
// records it. Push outcomes are stamped onto the Candidate's
// Pushed/PushError/ArtifactID/CipherSHA256 fields and re-appended
// to the ledger so the admin UI's history view reflects the push
// status alongside the discovery status.
type Manager struct {
	sched      *scheduler.Scheduler
	defaultLog string // default ledger path when BackupConfig.LedgerPath is empty

	mu     sync.Mutex
	cfg    conf.BackupConfig
	worker *Worker
	ledger *Ledger
	pusher *Pusher
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

// AttachPusher injects (or clears) the cloudbox pusher. Called at
// startup once main.go knows the cloudbox base + access token, and
// again whenever pairing changes. Pass nil to disable push.
func (m *Manager) AttachPusher(p *Pusher) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pusher = p
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
		out, err := m.worker.RunOnce(ctx, folders)
		if err != nil {
			return err
		}
		m.pushCandidates(ctx, out)
		return nil
	})
}

// pushCandidates iterates the worker's output and pushes each one
// to cloudbox (when a pusher is attached and the candidate is
// eligible — has bytes, hasn't been deduped, didn't error). Stamps
// push status onto the candidate and re-appends to the ledger so
// the admin UI surfaces "pushed: yes/no" alongside the discovery
// record.
//
// Push errors are recorded but do NOT propagate — one bad cloudbox
// upload shouldn't abort the worker for the rest of the folders.
// The next fire will retry candidates whose previous record wasn't
// Pushed=true (dedup keys off plaintext SHA in worker.runFolder).
func (m *Manager) pushCandidates(ctx context.Context, candidates []Candidate) {
	m.mu.Lock()
	pusher := m.pusher
	ledger := m.ledger
	m.mu.Unlock()
	if pusher == nil || !pusher.Configured() {
		return
	}
	for i := range candidates {
		c := &candidates[i]
		if c.Skipped || c.Error != "" || c.Path == "" {
			continue
		}
		app := appLabelFromFolder(c.Folder)
		res, err := pusher.Push(ctx, *c, app)
		if err != nil {
			c.PushError = err.Error()
		} else {
			c.Pushed = true
			c.ArtifactID = res.ArtifactID
			c.CipherSHA256 = res.CipherSHA256
		}
		// Re-append the candidate with push status so the ledger
		// reflects what actually happened. The original (worker-
		// only) row stays in place for forensic clarity.
		if ledger != nil {
			_ = ledger.Append(*c)
		}
	}
}

// appLabelFromFolder derives the "app" label cloudbox stores
// alongside the artifact. v1 just uses the folder basename
// (filepath.Base) — operators can rename folders to control the
// label. Documented in BackupConfig.Folders comment.
func appLabelFromFolder(folder string) string {
	base := filepathBase(folder)
	if base == "" {
		return "default"
	}
	return base
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
	out, err := worker.RunOnce(ctx, folders)
	if err != nil {
		return out, err
	}
	m.pushCandidates(ctx, out)
	return out, nil
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
