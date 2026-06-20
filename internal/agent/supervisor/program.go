// Package supervisor is a minimal process supervisor: it keeps a fixed set of
// child Programs alive — start them, restart on exit with capped backoff, and
// gracefully stop them on shutdown. It is the engine behind the
// `outpost supervisord` mode.
//
// This pass supervises exactly one program — the outpost daemon
// (`<self> start`) — but the API is list-shaped so a later pass can add
// managed routed apps (classgo, …) without restructuring. Ideas are borrowed
// from supervisord (per-program autorestart + startsecs healthy gate) and
// overseer (a persistent parent owning the child), but the implementation is
// deliberately small and dependency-free beyond the stdlib + errgroup.
package supervisor

import (
	"os"
	"time"
)

// Defaults for a Program's restart tuning.
const (
	DefaultStartSecs  = 5 * time.Second
	DefaultMinBackoff = 1 * time.Second
	DefaultMaxBackoff = 30 * time.Second
)

// Program is one supervised child process.
type Program struct {
	// Name identifies the program in logs and `supervisord status`.
	Name string
	// Path is the executable; Args are the arguments after it.
	Path string
	Args []string
	// Dir is the child's working directory. Empty = inherit the
	// supervisor's (load-bearing for apps that read assets relative to
	// CWD — e.g. classgo's templates/static, in a later pass).
	Dir string
	// Env is the full child environment. Nil = inherit os.Environ().
	Env []string
	// LogPath, when set, receives the child's combined stdout+stderr
	// (appended). Empty = inherit the supervisor's stdout/stderr.
	LogPath string

	// PreStart, when set, runs in the supervisor process immediately before
	// each launch of the child. It's the injection point for the
	// auto-rollback watchdog: inspect/repair on-disk state (e.g. revert a
	// binary that failed to confirm healthy) before the next boot. A
	// non-nil error is logged and the launch proceeds anyway — PreStart is
	// advisory, never a gate that could wedge the daemon down.
	PreStart func() error

	// StartSecs: a child that stays up at least this long is a healthy
	// start and resets the restart backoff. Zero = DefaultStartSecs.
	StartSecs time.Duration
	// MinBackoff / MaxBackoff bound the restart delay after a crash.
	// Zero = the Default* constants.
	MinBackoff time.Duration
	MaxBackoff time.Duration
}

func (p *Program) startSecs() time.Duration {
	if p.StartSecs > 0 {
		return p.StartSecs
	}
	return DefaultStartSecs
}

func (p *Program) minBackoff() time.Duration {
	if p.MinBackoff > 0 {
		return p.MinBackoff
	}
	return DefaultMinBackoff
}

func (p *Program) maxBackoff() time.Duration {
	if p.MaxBackoff > 0 {
		return p.MaxBackoff
	}
	return DefaultMaxBackoff
}

func (p *Program) env() []string {
	if p.Env != nil {
		return p.Env
	}
	return os.Environ()
}
