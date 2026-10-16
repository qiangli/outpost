package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// initReexecMarker is the second line of the init pidfile. Its presence says
// "this init re-execs itself when its binary is swapped", so `outpost
// upgrade` leaves it alone. An init that predates the marker (a bare pid
// line) keeps running its old code across every swap — on hosts where that
// code predates the init/monitor split it never starts `bashy supervisord`,
// and every process unit silently stays down — so upgrade hands it off.
const initReexecMarker = "reexec-on-swap"

// binarySwapInterval is how often init re-checks its own executable (and the
// bashy monitor's) for an upgrade swap.
const binarySwapInterval = 15 * time.Second

// errInitHandedOff is returned by reexecInit when a replacement init was
// spawned as a separate process (Windows); the caller simply exits.
var errInitHandedOff = errors.New("init handed off to a new process")

// reexecInit replaces this init with the binary now at self. Unix: exec in
// place, keeping the pid (and so the launchd/systemd registration and the
// pidfile). Windows: spawn a detached successor and exit. A var so tests
// never exec anything.
var reexecInit = reexecInitImpl

// readSupervisordPidFile parses the init pidfile: the pid, and whether that
// init declared it re-execs on a binary swap.
func readSupervisordPidFile() (pid int, reexecs bool, err error) {
	p, err := supervisordPidFilePath()
	if err != nil {
		return 0, false, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return 0, false, err
	}
	pid, reexecs = parseSupervisordPid(string(data))
	return pid, reexecs, nil
}

func parseSupervisordPid(data string) (pid int, reexecs bool) {
	lines := strings.Split(strings.TrimSpace(data), "\n")
	pid, _ = strconv.Atoi(strings.TrimSpace(lines[0]))
	for _, l := range lines[1:] {
		if strings.TrimSpace(l) == initReexecMarker {
			reexecs = true
		}
	}
	return pid, reexecs
}

// fileIdentity stats path after resolving symlinks, so a swap of the target
// a symlinked install points at is still seen.
func fileIdentity(path string) (os.FileInfo, error) {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	return os.Stat(path)
}

// watchBinarySwap blocks until one of paths names a different file than it
// did when the watch began (an upgrade's atomic rename replaces the inode),
// returning true, or until ctx is done, returning false. A path that cannot be
// stat'ed — absent at start, or mid-swap — is never a swap by itself.
func watchBinarySwap(ctx context.Context, paths []string, interval time.Duration, stat func(string) (os.FileInfo, error)) bool {
	base := make(map[string]os.FileInfo, len(paths))
	for _, p := range paths {
		if p == "" {
			continue
		}
		if fi, err := stat(p); err == nil {
			base[p] = fi
		}
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-t.C:
		}
		for p, was := range base {
			if now, err := stat(p); err == nil && !os.SameFile(was, now) {
				slog.Info("supervisord: binary replaced", "path", p)
				return true
			}
		}
	}
}

// superviseUntilSwap runs the children until ctx ends (swapped=false) or the
// watcher reports a swap, in which case it cancels run — which gracefully
// stops both children — and waits for it before returning swapped=true.
func superviseUntilSwap(ctx context.Context, run func(context.Context) error, watch func(context.Context) bool) (bool, error) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	swapped := make(chan bool, 1)
	go func() {
		s := watch(runCtx)
		swapped <- s
		if s {
			cancel() // stop both children so init can re-exec
		}
	}()
	err := run(runCtx)
	cancel()
	if s := <-swapped; s && ctx.Err() == nil {
		return true, nil
	}
	return false, err
}

// runInitLoop drives one supervise cycle after another. A swap re-execs init
// onto the new binary; if that exec fails, init keeps supervising the
// binaries on disk rather than leaving the host with no init at all.
func runInitLoop(ctx context.Context, cycle func(context.Context) (bool, error), reexec func() error) error {
	for {
		swapped, err := cycle(ctx)
		if !swapped {
			return err
		}
		slog.Info("supervisord: re-executing onto the replaced binary")
		rerr := reexec()
		if errors.Is(rerr, errInitHandedOff) {
			return nil
		}
		slog.Error("supervisord: re-exec failed; supervising again in this process", "err", rerr)
	}
}

// initHandoff restarts an init that cannot follow a binary swap itself. Every
// effect is a field so tests drive it without real processes.
type initHandoff struct {
	readPid   func() (pid int, reexecs bool, err error)
	alive     func(pid int) bool
	isOutpost func(pid int) bool
	stop      func(pid int) error
	spawn     func() error
	goos      string
	// exitWait bounds how long the old init gets to stop its children and
	// exit; reclaimWait is how long a service manager (launchd KeepAlive,
	// systemd Restart=) gets to bring up a replacement before we spawn one.
	exitWait, reclaimWait, poll time.Duration
}

// run returns one line per action for the operator. It only ever signals the
// pid named by init's own pidfile, and only when that pid is verifiably an
// outpost process — never a pattern match.
func (h initHandoff) run() []string {
	pid, reexecs, err := h.readPid()
	if err != nil || pid <= 0 || !h.alive(pid) {
		return nil // no init running: nothing predates the swap
	}
	if reexecs {
		return []string{fmt.Sprintf("init:     pid %d re-execs itself onto the new binary", pid)}
	}
	if pid == os.Getpid() || !h.isOutpost(pid) {
		return []string{fmt.Sprintf("init:     WARN pidfile names pid %d, which is not an outpost process; left alone", pid)}
	}
	if h.goos == "windows" {
		// Windows can only hard-kill init, which would orphan its children
		// rather than stop them; leave the restart to the operator.
		return []string{fmt.Sprintf("init:     WARN pid %d predates the swap and cannot start bashy supervisord; restart it (outpost service restart, or stop it and run outpost supervisord)", pid)}
	}
	out := []string{fmt.Sprintf("init:     pid %d predates the swap; restarting it so bashy supervisord runs", pid)}
	if err := h.stop(pid); err != nil {
		return append(out, fmt.Sprintf("init:     WARN stop pid %d: %v", pid, err))
	}
	if !h.waitFor(h.exitWait, func() bool { return !h.alive(pid) }) {
		return append(out, fmt.Sprintf("init:     WARN pid %d did not exit within %s; restart it by hand", pid, h.exitWait))
	}
	var next int
	if h.waitFor(h.reclaimWait, func() bool {
		np, _, err := h.readPid()
		if err == nil && np > 0 && np != pid && h.alive(np) {
			next = np
			return true
		}
		return false
	}) {
		return append(out, fmt.Sprintf("init:     restarted by the service manager (pid %d)", next))
	}
	if err := h.spawn(); err != nil {
		return append(out, fmt.Sprintf("init:     WARN start new init: %v — run outpost supervisord", err))
	}
	return append(out, "init:     started a new outpost supervisord")
}

func (h initHandoff) waitFor(d time.Duration, ok func() bool) bool {
	deadline := time.Now().Add(d)
	for {
		if ok() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(h.poll)
	}
}

// handOffLegacyInit is the production wiring of initHandoff for the binary
// `outpost upgrade` just swapped in.
func handOffLegacyInit(binary string) []string {
	return initHandoff{
		readPid:   readSupervisordPidFile,
		alive:     processAlive,
		isOutpost: pidIsOutpostStrict,
		stop: func(pid int) error {
			p, err := os.FindProcess(pid)
			if err != nil {
				return err
			}
			return p.Signal(syscall.SIGTERM)
		},
		spawn:       func() error { return spawnInit(binary) },
		goos:        runtime.GOOS,
		exitWait:    30 * time.Second,
		reclaimWait: 5 * time.Second,
		poll:        250 * time.Millisecond,
	}.run()
}

// pidIsOutpostStrict is pidIsOutpost without the benefit of the doubt: before
// signaling a process we must positively identify it.
func pidIsOutpostStrict(pid int) bool {
	name, err := processName(pid)
	return err == nil && name != "" && sameExecutableName(name)
}

// spawnInit starts a detached `<binary> supervisord`, logging next to the
// daemon's own log.
func spawnInit(binary string) error {
	c := exec.Command(binary, "supervisord")
	if dir, err := conf.DefaultCacheDir(); err == nil {
		if f, err := os.OpenFile(filepath.Join(dir, "supervisord.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			defer f.Close()
			c.Stdout, c.Stderr = f, f
		}
	}
	detach(c)
	if err := c.Start(); err != nil {
		return err
	}
	return c.Process.Release()
}

// initStatusWarnings explains a running init whose half of the control plane
// is missing. monitorErr is the result of asking bashy supervisord for unit
// status; an error there means no process unit is being kept up.
func initStatusWarnings(reexecs bool, monitorErr error) []string {
	var out []string
	if monitorErr != nil {
		out = append(out, fmt.Sprintf("WARN: init is running but the bashy supervisord service monitor is not (%v) — process units are not running; restart init", monitorErr))
	}
	if !reexecs {
		out = append(out, "WARN: this init predates self-re-exec on upgrade and keeps running old code; restart it (outpost upgrade does this for you)")
	}
	return out
}
