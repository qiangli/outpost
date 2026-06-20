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

		cmd := exec.CommandContext(ctx, p.Path, p.Args...)
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

		waitErr := cmd.Wait()
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

func (m *Manager) setRunning(name string, pid int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.status[name]
	s.Running = true
	s.PID = pid
	s.Starts++
}

func (m *Manager) setExited(name string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.status[name]
	s.Running = false
	s.PID = 0
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
