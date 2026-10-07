//go:build !windows

package shell

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

type terminal struct {
	ptm, pts  *os.File
	cmd       *exec.Cmd
	slaveOnce sync.Once
	slaveErr  error
}

func newTerminal() (*terminal, error) {
	master, slave, err := pty.Open()
	if err != nil {
		return nil, err
	}
	return &terminal{ptm: master, pts: slave}, nil
}
func (t *terminal) master() io.ReadWriteCloser { return t.ptm }
func (t *terminal) resize(cols, rows uint16) error {
	return pty.Setsize(t.ptm, &pty.Winsize{Cols: cols, Rows: rows})
}
func (t *terminal) start(ctx context.Context, path string, args, env []string) error {
	t.cmd = exec.CommandContext(ctx, path, args...)
	t.cmd.Env = env
	t.cmd.Stdin, t.cmd.Stdout, t.cmd.Stderr = t.pts, t.pts, t.pts
	// The child owns a controlling terminal, so signals and foreground jobs use
	// the kernel's ordinary terminal semantics without touching outpost's TTY.
	t.cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	t.cmd.Cancel = func() error {
		// Interactive job control can give the foreground job a different
		// process group from the shell. Terminate both owned groups; a job
		// deliberately detached with setsid/nohup is outside this session.
		t.killForeground(t.cmd.Process.Pid)
		return killProcessGroup(t.cmd.Process.Pid)
	}
	err := t.cmd.Start()
	if err == nil {
		_ = t.closeSlave()
	}
	return err
}
func (t *terminal) wait() error { return t.cmd.Wait() }
func (t *terminal) closeSlave() error {
	t.slaveOnce.Do(func() { t.slaveErr = t.pts.Close() })
	return t.slaveErr
}
func (t *terminal) close() error {
	// Do this before releasing the terminal descriptor: context cancellation is
	// asynchronous, and its callback otherwise may lose the foreground job ID.
	t.killForeground(0)
	_ = t.closeSlave()
	return t.ptm.Close()
}
func (t *terminal) killForeground(except int) {
	foreground, err := unix.IoctlGetInt(int(t.ptm.Fd()), unix.TIOCGPGRP)
	if err == nil && foreground > 0 && foreground != except && foreground != syscall.Getpgrp() {
		_ = syscall.Kill(-foreground, syscall.SIGKILL)
	}
}

// configureCommand isolates non-PTY requests so cancellation cannot kill the
// supervisor, while still reaching children inheriting Bashy's process group.
func configureCommand(cmd *exec.Cmd, isolate bool) {
	if !isolate {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killProcessGroup(cmd.Process.Pid) }
}
func killProcessGroup(pid int) error {
	err := syscall.Kill(-pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}
