//go:build linux

package overlay

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

var (
	restartInitialBackoff = 5 * time.Second
	restartMaxBackoff     = 60 * time.Second
)

// Run launches `tailscaled` and runs `tailscale up` to authenticate
// the node against the configured Headscale login server. Restarts
// tailscaled on unexpected exit; ctx cancellation is the only stop
// signal.
//
// Returns nil immediately when LoginServer is empty (overlay disabled)
// — caller treats that as "no overlay, proceed without it".
func Run(ctx context.Context, opts Options) error {
	if opts.LoginServer == "" {
		slog.Info("overlay: LoginServer empty; tailscaled disabled")
		return nil
	}
	if opts.AuthKey == "" {
		return errors.New("overlay: AuthKey required when LoginServer is set")
	}
	daemonBin := opts.Binary
	if daemonBin == "" {
		p, err := exec.LookPath("tailscaled")
		if err != nil {
			return fmt.Errorf("overlay: %w (install via `curl -fsSL https://tailscale.com/install.sh | sh`)", err)
		}
		daemonBin = p
	}
	tsBin := opts.TailscaleBinary
	if tsBin == "" {
		p, err := exec.LookPath("tailscale")
		if err != nil {
			return fmt.Errorf("overlay: %w (install via `curl -fsSL https://tailscale.com/install.sh | sh`)", err)
		}
		tsBin = p
	}
	stateDir := opts.StateDir
	if stateDir == "" {
		stateDir = DefaultStateDir
	}
	socketPath := opts.SocketPath
	if socketPath == "" {
		socketPath = DefaultSocketPath
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return fmt.Errorf("overlay: prepare state dir %s: %w", stateDir, err)
	}

	daemonArgs := []string{
		"--state=" + stateDir + "/tailscaled.state",
		"--socket=" + socketPath,
		"--statedir=" + stateDir,
		"--tun=tailscale0",
	}
	daemonArgs = append(daemonArgs, opts.ExtraDaemonArgs...)

	upArgs := []string{
		"--socket=" + socketPath,
		"up",
		"--login-server=" + opts.LoginServer,
		"--authkey=" + opts.AuthKey,
		"--reset",
	}
	accept := true
	if opts.AcceptRoutes != nil {
		accept = *opts.AcceptRoutes
	}
	if accept {
		upArgs = append(upArgs, "--accept-routes")
	}
	if len(opts.AdvertiseRoutes) > 0 {
		upArgs = append(upArgs, "--advertise-routes="+strings.Join(opts.AdvertiseRoutes, ","))
	}
	upArgs = append(upArgs, opts.ExtraUpArgs...)

	backoff := restartInitialBackoff
	for {
		if ctx.Err() != nil {
			return nil
		}
		runErr := runDaemonCycle(ctx, daemonBin, daemonArgs, tsBin, upArgs)
		if ctx.Err() != nil {
			return nil
		}
		switch {
		case runErr == nil:
			slog.Warn("overlay: tailscaled exited cleanly; restarting", "backoff", backoff)
		case errors.Is(runErr, context.Canceled):
			return nil
		default:
			slog.Warn("overlay: tailscaled exited with error; restarting", "err", runErr, "backoff", backoff)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = growBackoff(backoff)
	}
}

// runDaemonCycle starts tailscaled, runs `tailscale up` once it's
// listening, then blocks on the daemon's exit (or ctx). Returns the
// daemon's exit error (or context.Canceled when ctx fires first).
func runDaemonCycle(ctx context.Context, daemonBin string, daemonArgs []string, tsBin string, upArgs []string) error {
	daemon := exec.Command(daemonBin, daemonArgs...)
	daemon.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	daemon.Env = os.Environ()
	stdout, err := daemon.StdoutPipe()
	if err != nil {
		return fmt.Errorf("overlay: stdout pipe: %w", err)
	}
	stderr, err := daemon.StderrPipe()
	if err != nil {
		return fmt.Errorf("overlay: stderr pipe: %w", err)
	}
	slog.Info("overlay: starting tailscaled", "binary", daemonBin, "args", strings.Join(daemonArgs, " "))
	if err := daemon.Start(); err != nil {
		return fmt.Errorf("overlay: tailscaled start: %w", err)
	}
	pid := daemon.Process.Pid
	go streamToSlog(stdout, "tailscaled.stdout", pid)
	go streamToSlog(stderr, "tailscaled.stderr", pid)

	// Give tailscaled a moment to bind its socket before running
	// `tailscale up`. Polling the socket would be cleaner but a brief
	// fixed sleep matches the upstream installer's behavior and is
	// adequate; if `up` fails we surface it as a warning and the
	// supervisor loop retries.
	time.Sleep(2 * time.Second)
	up := exec.CommandContext(ctx, tsBin, upArgs...)
	upOut, upErr := up.CombinedOutput()
	if upErr != nil {
		slog.Warn("overlay: `tailscale up` failed; tailscaled keeps running, will retry on next supervisor cycle",
			"err", upErr, "output", strings.TrimSpace(string(upOut)))
	} else {
		slog.Info("overlay: `tailscale up` succeeded",
			"output", strings.TrimSpace(string(upOut)))
	}

	done := make(chan error, 1)
	go func() { done <- daemon.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = syscall.Kill(-pid, syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			slog.Warn("overlay: SIGTERM ignored, sending SIGKILL", "pid", pid)
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			<-done
		}
		return context.Canceled
	}
}

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
