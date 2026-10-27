package supervisor

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/qiangli/outpost/internal/jitter"
)

// gracefulStop is how long a child gets to exit after the platform stop
// signal before the os/exec machinery force-kills it on shutdown.
const gracefulStop = 10 * time.Second

// Status is a point-in-time view of one supervised program.
type Status struct {
	Name     string `json:"name"`
	Running  bool   `json:"running"`
	PID      int    `json:"pid,omitempty"`
	Starts   int    `json:"starts"`
	LastExit string `json:"last_exit,omitempty"`
	// Parked reports the program is held down by its ShouldRun gate
	// (a stop/disable intent) rather than crashed or starting.
	Parked bool `json:"parked,omitempty"`
}

// Manager supervises a fixed set of Programs, one goroutine each, until its
// Run context is canceled — at which point every child is gracefully stopped.
type Manager struct {
	programs []*Program
	grace    time.Duration

	mu     sync.Mutex
	status map[string]*Status
}

// New builds a Manager over the given programs.
func New(programs ...*Program) *Manager {
	m := &Manager{programs: programs, grace: gracefulStop, status: make(map[string]*Status, len(programs))}
	for _, p := range programs {
		m.status[p.Name] = &Status{Name: p.Name}
	}
	return m
}

// Run supervises every program, blocking until ctx is canceled (then all
// children are signaled + reaped) or a supervise loop returns a fatal error.
// A ctx-canceled shutdown returns nil.
func (m *Manager) Run(ctx context.Context) error {
	g, gctx := errgroup.WithContext(ctx)
	for _, p := range m.programs {
		g.Go(func() error { return m.superviseOne(gctx, p) })
	}
	if err := g.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// Snapshot returns the current state of each program, in declared order.
func (m *Manager) Snapshot() []Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Status, 0, len(m.programs))
	for _, p := range m.programs {
		out = append(out, *m.status[p.Name])
	}
	return out
}

// superviseOne runs one program forever: start → wait → (on crash) back off →
// restart, until ctx is canceled. A run that lasts at least StartSecs is
// "healthy" and resets the backoff; a fast crash grows it up to MaxBackoff.
func (m *Manager) superviseOne(ctx context.Context, p *Program) error {
	backoff := p.minBackoff()
	for {
		if ctx.Err() != nil {
			return nil
		}

		// Parked: an explicit stop/disable intent holds this program
		// down. Wait without launching (and without growing the
		// backoff) until the intent clears or the supervisor stops.
		if p.ShouldRun != nil && !p.ShouldRun() {
			m.setParked(p.Name)
			if !sleep(ctx, p.minBackoff()) {
				return nil
			}
			continue
		}
		m.clearParked(p.Name)

		// Watchdog hook: inspect/repair on-disk state before each launch
		// (auto-rollback reverts a binary that failed to confirm). Advisory
		// — an error is logged and we launch anyway, never wedging the host.
		if p.PreStart != nil {
			if err := p.PreStart(); err != nil {
				slog.Warn("supervisor: pre-start hook", "program", p.Name, "err", err)
			}
		}

		childCtx, stopChild := context.WithCancel(ctx)
		cmd := exec.CommandContext(childCtx, p.Path, p.Args...)
		cmd.Dir = p.Dir
		cmd.Env = p.env()
		// Graceful shutdown: on ctx cancel, os/exec calls Cancel (the
		// platform stop signal), then force-kills after WaitDelay if the
		// child is still alive. Stdout/Stderr go to a real file (or the
		// supervisor's), so there's no pipe-copier holding WaitDelay open.
		cmd.Cancel = func() error { return stopSignal(cmd.Process) }
		cmd.WaitDelay = m.grace

		var logf *os.File
		if p.LogPath != "" {
			if f, err := os.OpenFile(p.LogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
				logf = f
				cmd.Stdout, cmd.Stderr = f, f
			}
		}
		if cmd.Stdout == nil {
			cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		}

		startedAt := time.Now()
		if err := cmd.Start(); err != nil {
			stopChild()
			if logf != nil {
				_ = logf.Close()
			}
			m.setExited(p.Name, err)
			slog.Warn("supervisor: start failed", "program", p.Name, "err", err, "backoff", backoff)
			if !sleep(ctx, backoff) {
				return nil
			}
			backoff = jitter.Backoff(backoff, p.minBackoff(), p.maxBackoff())
			continue
		}
		m.setRunning(p.Name, cmd.Process.Pid)
		slog.Info("supervisor: started", "program", p.Name, "pid", cmd.Process.Pid)

		waitErr := waitForChild(cmd, p, stopChild)
		stopChild()
		if logf != nil {
			_ = logf.Close()
		}
		ranFor := time.Since(startedAt)
		m.setExited(p.Name, waitErr)

		if ctx.Err() != nil {
			return nil // shutdown, not a crash
		}
		if ranFor >= p.startSecs() {
			backoff = p.minBackoff() // healthy run → reset
		}
		slog.Warn("supervisor: program exited; restarting",
			"program", p.Name, "ranFor", ranFor.Round(time.Millisecond), "err", waitErr, "backoff", backoff)
		if !sleep(ctx, backoff) {
			return nil
		}
		if ranFor < p.startSecs() {
			backoff = jitter.Backoff(backoff, p.minBackoff(), p.maxBackoff())
		}
	}
}

// waitForChild polls only while this child is alive. Cancellation uses the
// same bounded graceful stop as supervisor shutdown, and Wait reaps the
// process before PreStart can replace its binary (also required on Windows).
func waitForChild(cmd *exec.Cmd, p *Program, stopChild context.CancelFunc) error {
	if p.RestartWhen == nil {
		return cmd.Wait()
	}
	interval := p.RestartCheckInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	for {
		select {
		case err := <-done:
			return err
		case <-ticker.C:
			if p.RestartWhen() {
				slog.Warn("supervisor: watchdog requested restart", "program", p.Name)
				stopChild()
				return <-done
			}
		}
	}
}

func (m *Manager) setRunning(name string, pid int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.status[name]
	s.Running = true
	s.PID = pid
	s.Parked = false
	s.Starts++
}

func (m *Manager) setParked(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.status[name]
	s.Running = false
	s.PID = 0
	s.Parked = true
	s.LastExit = "parked: stop/disable intent"
}

func (m *Manager) clearParked(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.status[name]
	if s.Parked {
		s.Parked = false
		s.LastExit = ""
	}
}

func (m *Manager) setExited(name string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.status[name]
	s.Running = false
	s.PID = 0
	s.Parked = false
	if err != nil {
		s.LastExit = err.Error()
	} else {
		s.LastExit = "exit 0"
	}
}

// sleep waits d or until ctx is canceled; returns false on cancel.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
