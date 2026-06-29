package shard

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"sync"
)

// LaunchConfig configures a prima process launch on this host.
type LaunchConfig struct {
	BinaryPath string    // path to the prima llama-cli / llama-server binary
	ModelPath  string    // -m <model>
	Extra      []string  // --prefetch, --gpu-mem, and for rank 0 the -p/-n prompt
	LogWriter  io.Writer // prima stdout+stderr sink (nil → discarded)
}

// Session is a running shard participant on this host: the prima process plus
// the mesh wiring it rides, torn down together by Stop.
type Session struct {
	cmd         *exec.Cmd
	cleanup     func()
	done        chan struct{}
	waitErr     error
	stopOnce    sync.Once
	cleanupOnce sync.Once
}

// Start wires this host's plan into the mesh forwarder, then launches prima
// against the local loopback ports that wiring owns (prima's --master/--next are
// 127.0.0.1, so every connection it makes is a mesh-forwarded loopback). It is
// fail-closed: if prima won't start, the mesh wiring is unwound before returning,
// so a half-formed shard never lingers.
func Start(ctx context.Context, f Forwarder, plan *HostPlan, cfg LaunchConfig) (*Session, error) {
	if cfg.BinaryPath == "" {
		return nil, fmt.Errorf("shard: empty prima binary path")
	}
	if cfg.ModelPath == "" {
		return nil, fmt.Errorf("shard: empty model path")
	}

	cleanup, err := Apply(f, plan)
	if err != nil {
		return nil, err
	}

	argv := plan.FullArgs(cfg.ModelPath, cfg.Extra...)
	cmd := exec.CommandContext(ctx, cfg.BinaryPath, argv...)
	if cfg.LogWriter != nil {
		cmd.Stdout = cfg.LogWriter
		cmd.Stderr = cfg.LogWriter
	}
	if err := cmd.Start(); err != nil {
		cleanup()
		return nil, fmt.Errorf("shard: start prima (rank %d): %w", plan.MyRank, err)
	}

	s := &Session{cmd: cmd, cleanup: cleanup, done: make(chan struct{})}
	go func() {
		s.waitErr = cmd.Wait()
		close(s.done)
	}()
	return s, nil
}

// Wait blocks until the prima process exits on its own and returns its exit
// error (nil on a clean exit).
func (s *Session) Wait() error {
	<-s.done
	return s.waitErr
}

// Running reports whether the prima process is still alive.
func (s *Session) Running() bool {
	select {
	case <-s.done:
		return false
	default:
		return true
	}
}

// Stop kills the prima process, waits for it to reap (draining its log), then
// tears down the mesh wiring. Idempotent — safe to call more than once and
// alongside a Wait.
func (s *Session) Stop() {
	s.stopOnce.Do(func() {
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
	})
	<-s.done
	s.cleanupOnce.Do(s.cleanup)
}
