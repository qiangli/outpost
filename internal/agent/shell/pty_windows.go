//go:build windows

package shell

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	xpty "github.com/aymanbagabas/go-pty"
	"golang.org/x/sys/windows"
)

// ConPTY attaches the real Bashy child to a console. The former in-process
// pipe emulation cannot give a child terminal input, resize, or Ctrl-C.
type terminal struct {
	pty        xpty.Pty
	cmd        *xpty.Cmd
	closeOnce  sync.Once
	closeErr   error
	stopCancel func() bool
	cancelDone chan struct{}
	cancelErr  error
}

func newTerminal() (*terminal, error) {
	p, err := xpty.New()
	if err != nil {
		return nil, err
	}
	return &terminal{pty: p}, nil
}
func (t *terminal) master() io.ReadWriteCloser     { return t.pty }
func (t *terminal) resize(cols, rows uint16) error { return t.pty.Resize(int(cols), int(rows)) }
func (t *terminal) start(ctx context.Context, path string, args, env []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Own cancellation here: the pinned go-pty CommandContext implementation
	// does not join its context watcher before Wait reads cancellation state.
	t.cmd = t.pty.Command(path, args...)
	t.cmd.Env = env
	if err := t.cmd.Start(); err != nil {
		return err
	}
	t.cancelDone = make(chan struct{})
	t.stopCancel = context.AfterFunc(ctx, func() {
		defer close(t.cancelDone)
		treeErr := terminateProcessTree(t.cmd.Process)
		closeErr := t.close()
		t.cancelErr = errors.Join(ctx.Err(), treeErr, closeErr)
	})
	return nil
}
func (t *terminal) wait() error {
	err := t.cmd.Wait()
	if !t.stopCancel() {
		<-t.cancelDone
		err = errors.Join(err, t.cancelErr)
	}
	return err
}

func (t *terminal) closeSlave() error {
	t.closeOnce.Do(func() { t.closeErr = t.pty.Close() })
	return t.closeErr
}
func (t *terminal) close() error {
	// Start the abort drain before waiting on closeOnce: a graceful CloseSlave
	// may already be flushing while its transport reader has disconnected.
	go func() { _, _ = io.Copy(io.Discard, t.pty) }()
	t.closeOnce.Do(func() { t.closeErr = t.pty.Close() })
	return t.closeErr
}

func configureCommand(cmd *exec.Cmd, isolate bool) {
	if !isolate {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
	cmd.Cancel = func() error { return terminateProcessTree(cmd.Process) }
}

// Reuse the native taskkill /T strategy already used by outpost's Windows
// service processes. Resolve the OS utility independently of a daemon's PATH;
// this is process management, never a fallback shell for user commands.
func terminateProcessTree(process *os.Process) error {
	if process == nil {
		return nil
	}
	dir, err := windows.GetSystemDirectory()
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		kill := exec.CommandContext(ctx, filepath.Join(dir, "taskkill.exe"), "/T", "/F", "/PID", strconv.Itoa(process.Pid))
		kill.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		err = kill.Run()
		if err == nil {
			return nil
		}
	}
	// Always reap the direct child even if the OS tree utility failed. Surface
	// that failure so callers cannot mistake partial cancellation for success.
	directErr := process.Kill()
	if errors.Is(directErr, os.ErrProcessDone) {
		return os.ErrProcessDone
	}
	return errors.Join(err, directErr)
}
