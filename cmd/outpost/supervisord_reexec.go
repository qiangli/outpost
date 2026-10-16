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

// procEntry is one row of the process table: pid and full command line.
type procEntry struct {
	pid  int
	args string
}

// initProbe is what discovery found about the running init. source says
// where the pid came from, for the operator.
type initProbe struct {
	pid     int
	reexecs bool
	source  string
}

// initDiscovery locates the running outpost init. It is THE discovery path:
// `outpost supervisord status` and the upgrade handoff both go through it, so
// the two can never disagree about whether an init is running. Every effect
// is a field so tests drive it without real processes.
type initDiscovery struct {
	pidFiles func() []string
	readFile func(string) ([]byte, error)
	alive    func(int) bool
	scan     func() ([]procEntry, error)
	self     int
}

// find returns the running init, or found=false with a one-phrase reason.
// Order: the init pidfile (current, then the pre-XDG legacy cache dir), then
// the process table — an init from before the pidfile moved, or whose
// pidfile was lost, is still running and still needs handing off. A pidfile
// found that way carries no re-exec marker, and neither does a bare process:
// every init that re-execs on a swap writes the marker into its pidfile.
func (d initDiscovery) find() (p initProbe, found bool, reason string) {
	reason = "no pidfile"
	for _, path := range d.pidFiles() {
		data, err := d.readFile(path)
		if err != nil {
			continue
		}
		pid, reexecs := parseSupervisordPid(string(data))
		if pid > 0 && pid != d.self && d.alive(pid) {
			return initProbe{pid: pid, reexecs: reexecs, source: "pidfile " + path}, true, ""
		}
		reason = fmt.Sprintf("stale pidfile, last pid %d", pid)
	}
	procs, err := d.scan()
	if err != nil {
		return initProbe{}, false, reason
	}
	for _, pr := range procs {
		if pr.pid > 0 && pr.pid != d.self && isSupervisordInvocation(pr.args) && d.alive(pr.pid) {
			return initProbe{pid: pr.pid, source: "process table"}, true, ""
		}
	}
	return initProbe{}, false, reason
}

// isSupervisordInvocation reports whether a command line runs an outpost
// binary as `supervisord` itself — not one of its subcommands such as
// `supervisord status`, which is a short-lived client.
func isSupervisordInvocation(args string) bool {
	f := strings.Fields(args)
	if len(f) < 2 || !sameExecutableName(f[0]) {
		return false
	}
	for i, a := range f[1:] {
		if a != "supervisord" {
			continue
		}
		rest := f[i+2:]
		return len(rest) == 0 || strings.HasPrefix(rest[0], "-")
	}
	return false
}

// discoverInit is the production initDiscovery.
func discoverInit() (initProbe, bool, string) {
	return initDiscovery{
		pidFiles: supervisordPidFileCandidates,
		readFile: os.ReadFile,
		alive:    processAlive,
		scan:     listProcesses,
		self:     os.Getpid(),
	}.find()
}

// supervisordPidFileCandidates lists where an init may have written its
// pidfile: the current cache dir, then the pre-XDG one.
func supervisordPidFileCandidates() []string {
	var out []string
	if p, err := supervisordPidFilePath(); err == nil {
		out = append(out, p)
	}
	if dir, err := conf.LegacyCacheDir(); err == nil {
		if p := filepath.Join(dir, "supervisord.pid"); len(out) == 0 || p != out[0] {
			out = append(out, p)
		}
	}
	return out
}

// initHandoff restarts an init that cannot follow a binary swap itself. Every
// effect is a field so tests drive it without real processes.
type initHandoff struct {
	discover      func() (initProbe, bool, string)
	alive         func(pid int) bool
	isOutpost     func(pid int) bool
	isSupervisord func(pid int) bool
	stop          func(pid int) error
	spawn         func() error
	goos          string
	// exitWait bounds how long the old init gets to stop its children and
	// exit; reclaimWait is how long a service manager (launchd KeepAlive,
	// systemd Restart=) gets to bring up a replacement before we spawn one.
	exitWait, reclaimWait, poll time.Duration
}

// run returns the operator report — always at least one line saying what it
// found and what it did. It only signals a pid that discovery found AND that
// is positively identified as an outpost process running `supervisord`.
func (h initHandoff) run() []string {
	probe, found, reason := h.discover()
	if !found {
		return []string{fmt.Sprintf("init:     no running init (%s)", reason)}
	}
	pid := probe.pid
	skip := func(why string) []string {
		return []string{fmt.Sprintf("init:     skipped: pid %d (%s) %s", pid, probe.source, why)}
	}
	if probe.reexecs {
		return skip("re-execs itself onto the new binary")
	}
	if pid == os.Getpid() || !h.isOutpost(pid) {
		return skip("is not an outpost process; left alone")
	}
	if !h.isSupervisord(pid) {
		return skip("is not an outpost supervisord invocation; left alone")
	}
	if h.goos == "windows" {
		// Windows can only hard-kill init, which would orphan its children
		// rather than stop them; leave the restart to the operator.
		return skip("predates the swap and cannot start bashy supervisord; WARN restart it (outpost service restart, or stop it and run outpost supervisord)")
	}
	out := []string{fmt.Sprintf("init:     pid %d (%s) predates the swap; restarting it so bashy supervisord runs", pid, probe.source)}
	if err := h.stop(pid); err != nil {
		return append(out, fmt.Sprintf("init:     skipped: WARN stop pid %d: %v", pid, err))
	}
	if !h.waitFor(h.exitWait, func() bool { return !h.alive(pid) }) {
		return append(out, fmt.Sprintf("init:     skipped: WARN pid %d did not exit within %s; restart it by hand", pid, h.exitWait))
	}
	var next int
	if h.waitFor(h.reclaimWait, func() bool {
		np, ok, _ := h.discover()
		if ok && np.pid != pid {
			next = np.pid
			return true
		}
		return false
	}) {
		return append(out, fmt.Sprintf("init:     handed off pid %d; restarted by the service manager (pid %d)", pid, next))
	}
	if err := h.spawn(); err != nil {
		return append(out, fmt.Sprintf("init:     WARN pid %d stopped but starting a new init failed: %v — run outpost supervisord", pid, err))
	}
	return append(out, fmt.Sprintf("init:     handed off pid %d; started a new outpost supervisord", pid))
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
		discover:  discoverInit,
		alive:     processAlive,
		isOutpost: pidIsOutpostStrict,
		isSupervisord: func(pid int) bool {
			args, err := processArgs(pid)
			return err == nil && isSupervisordInvocation(args)
		},
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
