package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/supervisor"
)

// envSupervised is set to "1" in the daemon's env by the supervisor. The
// daemon reads it (execSelfStart) to choose exit-and-be-respawned over
// detached self-re-exec.
const envSupervised = "OUTPOST_SUPERVISED"

// supervisordCmd is the always-up component of the two-part binary: a tiny
// parent that keeps the outpost daemon (`<self> start`) alive — start it,
// restart it on exit, stop it on shutdown. The OS service (`outpost service
// install`) registers THIS command, so the supervisor — and through it the
// daemon — survive a reboot. This pass supervises only the daemon; a later
// pass adds managed routed apps via the same supervisor.Manager.
//
// The daemon is launched with OUTPOST_SUPERVISED=1 in its env, which flips
// its restart sites from self-re-exec (execSelfStart) to exit-and-let-the-
// supervisor-respawn — so a config change or a post-upgrade swap just exits
// the daemon and the supervisor brings the new one up under the same parent.
func supervisordCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "supervisord",
		Short: "Run the always-up supervisor that keeps the outpost daemon alive (registered by `outpost service install`)",
		Long: `supervisord is the resilient parent half of the outpost binary. It runs
the outpost daemon as a supervised child — restarting it on exit with a
capped backoff — and is what the OS boot service launches so everything
survives a reboot. Run in the foreground; the launchd/systemd/Task-Scheduler
entry created by 'outpost service install' keeps it up.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSupervisord(cmd.Context())
		},
	}
	cmd.AddCommand(supervisordStatusCmd())
	return cmd
}

func runSupervisord(ctx context.Context) error {
	if err := claimSupervisordPidFile(); err != nil {
		return err
	}
	defer removeSupervisordPidFile()

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve own path: %w", err)
	}

	// The daemon's stdout/stderr land in a log next to its pidfile so the
	// supervisor's own OS-service log stays uncluttered.
	logPath := ""
	if dir, derr := conf.DefaultCacheDir(); derr == nil {
		logPath = filepath.Join(dir, "daemon.log")
	}

	daemon := &supervisor.Program{
		Name:    "outpost",
		Path:    self,
		Args:    []string{"start"},
		Env:     append(os.Environ(), envSupervised+"=1"),
		LogPath: logPath,
	}
	// Auto-rollback watchdog: before each (re)launch, revert a just-upgraded
	// binary that failed to confirm healthy. No-op without a pending upgrade;
	// the destructive revert is gated by auto_rollback_enabled (default off).
	if dir, derr := conf.DefaultCacheDir(); derr == nil {
		daemon.PreStart = watchdogPreStart(dir)
	}

	// Translate SIGINT/SIGTERM into a context cancel so the supervisor
	// gracefully stops the daemon before we exit.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	slog.Info("supervisord: starting", "binary", self, "daemon_log", logPath)
	mgr := supervisor.New(daemon)
	err = mgr.Run(ctx)
	slog.Info("supervisord: stopped")
	return err
}

// supervisordStatusCmd reports whether a supervisor is running (via its
// pidfile). Full per-program state would need an IPC surface on the running
// supervisor — out of scope for this pass.
func supervisordStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report whether the outpost supervisor is running",
		RunE: func(_ *cobra.Command, _ []string) error {
			p, err := supervisordPidFilePath()
			if err != nil {
				return err
			}
			data, err := os.ReadFile(p)
			if err != nil {
				fmt.Println("supervisord: not running (no pidfile)")
				return nil
			}
			pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
			if pid > 0 && processAlive(pid) {
				fmt.Printf("supervisord: running (pid %d)\n", pid)
			} else {
				fmt.Printf("supervisord: not running (stale pidfile, last pid %d)\n", pid)
			}
			return nil
		},
	}
}

// supervisordPidFilePath mirrors pidFilePath() but for the supervisor, so the
// two single-instance guards never collide.
func supervisordPidFilePath() (string, error) {
	dir, err := conf.DefaultCacheDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, "supervisord.pid"), nil
}

func claimSupervisordPidFile() error {
	p, err := supervisordPidFilePath()
	if err != nil {
		return err
	}
	if data, err := os.ReadFile(p); err == nil {
		if oldPid, perr := strconv.Atoi(strings.TrimSpace(string(data))); perr == nil && oldPid > 0 && processAlive(oldPid) {
			return fmt.Errorf("outpost supervisord is already running (pid %d)", oldPid)
		}
	}
	return os.WriteFile(p, []byte(strconv.Itoa(os.Getpid())), 0o600)
}

func removeSupervisordPidFile() {
	if p, err := supervisordPidFilePath(); err == nil {
		_ = os.Remove(p)
	}
}
