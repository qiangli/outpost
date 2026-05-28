//go:build linux

package k3sagent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Reconnect tuning for the supervisor loop. Vars (not consts) so tests
// can shrink them; production paths shouldn't touch.
var (
	restartInitialBackoff = 5 * time.Second
	restartMaxBackoff     = 60 * time.Second
)

// Run launches `k3s agent` as a child process and restarts it on
// unexpected exit until ctx is canceled. Blocks. Returns nil when ctx
// is canceled, non-nil only for unrecoverable setup failures (binary
// not found, options invalid).
//
// The agent's logs are streamed to slog with level=info — same place
// as the rest of the outpost daemon's structured output.
func Run(ctx context.Context, opts Options) error {
	if err := opts.validate(); err != nil {
		return err
	}
	bin := opts.Binary
	if bin == "" {
		p, err := exec.LookPath("k3s")
		if err != nil {
			return fmt.Errorf("k3sagent: %w (install per docs, e.g. `curl -sfL https://get.k3s.io | INSTALL_K3S_SKIP_ENABLE=true sh -`)", err)
		}
		bin = p
	}
	dataDir := opts.DataDir
	if dataDir == "" {
		dataDir = DefaultDataDir
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("k3sagent: prepare data dir %s: %w", dataDir, err)
	}

	args := []string{
		"agent",
		"--server=" + opts.Server,
		"--token=" + opts.Token,
		"--data-dir=" + dataDir,
		"--node-name=" + opts.NodeName,
	}
	args = append(args, opts.ExtraArgs...)

	backoff := restartInitialBackoff
	for {
		if ctx.Err() != nil {
			return nil
		}
		runErr := runOnce(ctx, bin, args)
		if ctx.Err() != nil {
			return nil
		}
		switch {
		case runErr == nil:
			slog.Warn("k3sagent: process exited cleanly; restarting", "backoff", backoff)
		case errors.Is(runErr, context.Canceled):
			return nil
		default:
			slog.Warn("k3sagent: process exited with error; restarting", "err", runErr, "backoff", backoff)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = growBackoff(backoff)
	}
}

// runOnce executes the agent once, streaming stdout/stderr to slog.
// Returns nil on clean exit (rare for a long-lived daemon — usually
// indicates a config rejection k3s logged on the way out) and an error
// otherwise. ctx cancellation triggers SIGTERM then SIGKILL after a
// 10s grace period.
func runOnce(ctx context.Context, bin string, args []string) error {
	cmd := exec.Command(bin, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// k3s agent uses kubelet and containerd which both like a tmpfs
	// they can write to without colliding with anything else; falling
	// back to the parent env is fine.
	cmd.Env = os.Environ()

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("k3sagent: stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("k3sagent: stderr pipe: %w", err)
	}

	slog.Info("k3sagent: starting", "binary", bin, "args", strings.Join(args, " "))
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("k3sagent: start: %w", err)
	}
	pid := cmd.Process.Pid

	go streamToSlog(stdoutPipe, "k3sagent.stdout", pid)
	go streamToSlog(stderrPipe, "k3sagent.stderr", pid)

	// Watch ctx so we can SIGTERM the subprocess on cancel without
	// waiting for the natural exit (which could be never for a
	// healthy kubelet).
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		// Politely ask the process group to exit, then escalate.
		_ = syscall.Kill(-pid, syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			slog.Warn("k3sagent: SIGTERM ignored, sending SIGKILL", "pid", pid)
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			<-done
		}
		return context.Canceled
	}
}

// streamToSlog forwards lines from r to slog at info level. Errors
// (closed pipe etc.) terminate the goroutine quietly — the caller
// supervises the subprocess lifecycle and will see Wait() return.
func streamToSlog(r interface {
	Read(p []byte) (int, error)
	Close() error
}, source string, pid int) {
	defer r.Close()
	buf := make([]byte, 8192)
	var carry []byte
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := append(carry, buf[:n]...)
			lines := strings.Split(string(chunk), "\n")
			carry = []byte(lines[len(lines)-1])
			for _, line := range lines[:len(lines)-1] {
				if line == "" {
					continue
				}
				slog.Info(source, "pid", pid, "line", line)
			}
		}
		if err != nil {
			if len(carry) > 0 {
				slog.Info(source, "pid", pid, "line", string(carry))
			}
			return
		}
	}
}

func growBackoff(cur time.Duration) time.Duration {
	next := cur * 2
	if next > restartMaxBackoff {
		return restartMaxBackoff
	}
	return next
}

func (o Options) validate() error {
	if o.Server == "" {
		return errors.New("k3sagent: Options.Server required")
	}
	if o.Token == "" {
		return errors.New("k3sagent: Options.Token required")
	}
	if o.NodeName == "" {
		return errors.New("k3sagent: Options.NodeName required")
	}
	if o.DataDir != "" {
		if !filepath.IsAbs(o.DataDir) {
			return fmt.Errorf("k3sagent: Options.DataDir %q must be absolute", o.DataDir)
		}
	}
	return nil
}
