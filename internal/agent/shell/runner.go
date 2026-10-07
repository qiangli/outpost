// Package shell runs the paired bashy executable behind a real terminal.
// Outpost owns process and terminal lifecycle; Bashy owns shell behavior.
package shell

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/qiangli/outpost/internal/bashypath"
)

type SessionOptions struct {
	Term       string
	Cols, Rows uint16
	Env        map[string]string
}

// Session owns a terminal and at most one bashy child. Run and RunOnce are
// mutually exclusive; Close cancels the child and releases the terminal.
type Session struct {
	terminal        *terminal
	path            string
	env             []string
	done            chan struct{}
	mu              sync.Mutex
	started, closed bool
	cancel          context.CancelFunc
}

func NewSession(opts SessionOptions) (*Session, error) {
	path, _, err := bashypath.Find(nil)
	if err != nil {
		return nil, err
	}
	terminal, err := newTerminal()
	if err != nil {
		return nil, fmt.Errorf("open pty: %w", err)
	}
	overrides := make(map[string]string, len(opts.Env)+1)
	for k, v := range opts.Env {
		overrides[k] = v
	}
	if opts.Term != "" {
		overrides["TERM"] = opts.Term
	} else if _, ok := overrides["TERM"]; !ok {
		overrides["TERM"] = "xterm-256color"
	}
	if opts.Cols == 0 {
		opts.Cols = 80
	}
	if opts.Rows == 0 {
		opts.Rows = 24
	}
	if err = terminal.resize(opts.Cols, opts.Rows); err != nil {
		_ = terminal.close()
		return nil, err
	}
	return &Session{terminal: terminal, path: path, env: BuildEnvWith(overrides), done: make(chan struct{})}, nil
}

func (s *Session) Master() io.ReadWriteCloser { return s.terminal.master() }
func (s *Session) Done() <-chan struct{}      { return s.done }
func (s *Session) Resize(cols, rows uint16) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("shell session closed")
	}
	return s.terminal.resize(cols, rows)
}
func (s *Session) Run(ctx context.Context) error { return s.run(ctx, []string{"-i"}) }
func (s *Session) RunOnce(ctx context.Context, command string) uint32 {
	code, err := exitCodeFromError(s.run(ctx, []string{"-c", command}))
	if err != nil {
		return 1
	}
	return uint32(code)
}
func (s *Session) run(ctx context.Context, args []string) error {
	s.mu.Lock()
	if s.closed || s.started {
		s.mu.Unlock()
		return errors.New("shell session already started or closed")
	}
	s.started = true
	ctx, s.cancel = context.WithCancel(ctx)
	err := s.terminal.start(ctx, s.path, args, s.env)
	s.mu.Unlock()
	defer close(s.done)
	defer s.cancel()
	if err != nil {
		return fmt.Errorf("start bashy: %w", err)
	}
	return s.terminal.wait()
}
func (s *Session) CloseSlave() error { return s.terminal.closeSlave() }
func (s *Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.cancel != nil {
		s.cancel()
	}
	return s.terminal.close()
}
