package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// emitStartupTrace appends one best-effort line to cli-trace.log
// recording the invocation context (timestamp, argv, pid, ppid, cwd,
// PATH). Errors are swallowed — this is post-mortem instrumentation,
// it must never block or fail the CLI.
//
// Why: when an outpost CLI invocation is killed unusually early
// (shell hook that intercepts certain absolute paths, sandbox limit,
// LSM, etc.) the daemon log and stderr show nothing — the process
// never reached normal logging. One durable line per invocation
// proves main() was reached and records exactly what the kernel saw,
// which is enough to diagnose a "outpost died in 1 ms with signal 137
// and no output" report.
//
// CLI-scoped on purpose: not mixed into the daemon's outpost.log,
// which is the gin/slog stream and would be polluted by short-lived
// CLI lines. Set OUTPOST_TRACE=0 to opt out.
func emitStartupTrace() {
	if os.Getenv("OUTPOST_TRACE") == "0" {
		return
	}
	dir, err := conf.DefaultCacheDir()
	if err != nil {
		return
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "cli-trace.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()

	wd, _ := os.Getwd()
	// Single Write call so the line lands atomically in the kernel
	// page cache — survives SIGKILL of this process even if the
	// deferred Close never runs.
	_, _ = fmt.Fprintf(f,
		"%s pid=%d ppid=%d wd=%q argv0=%q argv=%q path=%q\n",
		time.Now().Format(time.RFC3339Nano),
		os.Getpid(),
		os.Getppid(),
		wd,
		os.Args[0],
		strings.Join(os.Args[1:], " "),
		os.Getenv("PATH"),
	)
}
