// Package shell is the in-process bash interpreter (qiangli/sh / mvdan.cc/sh)
// wrapped in a PTY so xterm.js sees a real TTY: line discipline, echo,
// backspace, resize, and Ctrl-C all flow through the kernel TTY layer
// just as they would for a child `bash` process — except there is no
// child process.
package shell

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// Session is one interactive shell sitting between a tty pair and a runner.
// Caller writes to / reads from the master side of the PTY; the runner is
// hooked up to the slave side as stdin/stdout/stderr.
type Session struct {
	ptm    *os.File // master (caller side)
	pts    *os.File // slave (runner side)
	runner *interp.Runner
	done   chan struct{}
}

// SessionOptions configures a new shell Session. All fields are optional —
// the zero value is "no PTY hints, inherit outpost's env verbatim", which
// matches the pre-options behavior used by the xterm.js /shell path.
type SessionOptions struct {
	// Term is the TERM env var the runner should see (e.g. "xterm-256color"
	// from an SSH pty-req). Empty = inherit outpost's TERM (usually unset
	// in a daemon context, which makes vim/htop fall back to dumb mode).
	Term string
	// Cols/Rows are the initial PTY window dimensions in characters.
	// Both 0 = skip the initial resize.
	Cols uint16
	Rows uint16
	// Env is an optional set of env-var overrides applied on top of
	// the daemon's env. The SSH server uses this to stamp
	// SSH_AUTH_SOCK from per-session agent forwarding (`ssh -A`).
	// Empty/nil = no overrides.
	Env map[string]string
}

// NewSession allocates a PTY pair and constructs the runner. Caller is
// responsible for closing the returned Session.
func NewSession(opts SessionOptions) (*Session, error) {
	ptm, pts, err := openPTY()
	if err != nil {
		return nil, fmt.Errorf("open pty: %w", err)
	}

	// Merge Term + caller-supplied env overrides (e.g. SSH_AUTH_SOCK
	// from `ssh -A`) into a single overrides map, then build the env
	// once. Nil/empty input is fine — BuildEnvWith treats it as no
	// overrides.
	var overrides map[string]string
	if opts.Term != "" || len(opts.Env) > 0 {
		overrides = make(map[string]string, len(opts.Env)+1)
		for k, v := range opts.Env {
			overrides[k] = v
		}
		if opts.Term != "" {
			overrides["TERM"] = opts.Term
		}
	}
	env := BuildEnvWith(overrides)

	runner, err := interp.New(
		interp.StdIO(pts, pts, pts),
		interp.Env(env), // outpost process env + user-shell-style PATH extras (+ TERM if hinted)
		interp.WithBgPidCallback(func(pid int) {
			// Cmd is "(detached)" because the fork's callback signature is
			// (pid int) only; richer capture would need stmt threading in
			// publishBgPid. See docs/matrix-shell-outpost-handoff.md.
			_ = DefaultRegistry().Record(pid, "(detached)")
		}),
	)
	if err != nil {
		_ = ptm.Close()
		_ = pts.Close()
		return nil, fmt.Errorf("interp: %w", err)
	}
	if opts.Cols > 0 && opts.Rows > 0 {
		// Apply geometry before the runner's first read so the very first
		// `tput cols` / ioctl(TIOCGWINSZ) sees the client's window.
		_ = setPTYSize(ptm, opts.Cols, opts.Rows)
	}
	return &Session{ptm: ptm, pts: pts, runner: runner, done: make(chan struct{})}, nil
}

// Master returns the master fd. The caller pipes WebSocket bytes ↔ this.
func (s *Session) Master() *os.File { return s.ptm }

// RunOnce parses `command` and runs it once through the PTY-backed
// runner, then returns. Used by the SSH `exec` path when the client
// asked for a TTY first (`ssh -tt host cmd`) — the command sees a real
// /dev/ttysNN so `tty`, `screen -dmS`, etc. behave like they do under
// real openssh. Caller pipes the channel ↔ s.Master() and tears down
// the session when RunOnce returns.
//
// Returns a POSIX-style exit status (0 = ok, non-zero from the
// command or from a parse error → 127 / 1).
func (s *Session) RunOnce(ctx context.Context, command string) uint32 {
	defer close(s.done)

	parser := syntax.NewParser()
	file, err := parser.Parse(strings.NewReader(command), "")
	if err != nil {
		_, _ = io.WriteString(s.pts, err.Error()+"\r\n")
		return 127
	}
	if err := s.runner.Run(ctx, file); err != nil {
		var ec interp.ExitStatus
		if errorsAs(err, &ec) {
			return uint32(ec)
		}
		_, _ = io.WriteString(s.pts, err.Error()+"\r\n")
		return 1
	}
	return 0
}

// Resize updates the PTY's window size — equivalent to a SIGWINCH inside
// the runner. cols/rows in characters.
func (s *Session) Resize(cols, rows uint16) error { return setPTYSize(s.ptm, cols, rows) }

// Run starts the interactive parse-and-execute loop, blocking until ctx is
// canceled, the parser hits EOF on the PTY slave, or a fatal interp error.
// Each parsed statement runs against a child context so a Ctrl-C cancels
// just the current command, not the whole shell.
func (s *Session) Run(ctx context.Context) error {
	defer close(s.done)

	greeting := "Matrix shell (qiangli/sh in-process) — type `exit` or close the tab.\r\n"
	_, _ = io.WriteString(s.pts, greeting)

	parser := syntax.NewParser()
	// Emit PS1 before the first read; thereafter PS1/PS2 are emitted from
	// the callback. Pattern mirrors the example in syntax.Parser.InteractiveSeq.
	_, _ = io.WriteString(s.pts, ps1(s.runner))

	return parser.Interactive(s.pts, func(stmts []*syntax.Stmt) bool {
		if parser.Incomplete() {
			// Multi-line statement still being typed (open quote, then-block,
			// trailing pipe, …). Emit the continuation prompt and keep
			// reading without running anything yet.
			_, _ = io.WriteString(s.pts, ps2())
			return true
		}
		for _, stmt := range stmts {
			cmdCtx, cancel := context.WithCancel(ctx)
			err := s.runner.Run(cmdCtx, stmt)
			cancel()
			if err != nil && !isExitStatus(err) {
				// A real fatal error from a handler (not just a non-zero
				// command exit code). Surface it like a real shell would.
				// Plain `exit status N` from failing commands is intentionally
				// swallowed — the command's own stderr already told the user
				// what went wrong, and printing "exit status 127" on top is
				// just noise.
				_, _ = io.WriteString(s.pts, err.Error()+"\r\n")
			}
			if s.runner.Exited() {
				// The `exit` builtin (or a fatal trap) asked us to end the
				// session. Treat it like Ctrl-D.
				return false
			}
		}
		// Continue while the parent ctx is alive.
		select {
		case <-ctx.Done():
			return false
		default:
			_, _ = io.WriteString(s.pts, ps1(s.runner))
			return true
		}
	})
}

// Close releases the PTY pair. Safe to call multiple times.
func (s *Session) Close() error {
	var firstErr error
	if err := s.ptm.Close(); err != nil {
		firstErr = err
	}
	if err := s.pts.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// CloseSlave closes the slave (runner-side) PTY fd only, leaving the
// master open so a reader can drain any kernel-buffered output. Used
// by the SSH exec-with-pty path: after the runner finishes we close
// the slave to signal EOF, wait for the PTY→channel goroutine to
// drain, then Close() the rest. Closing the master prematurely would
// drop bytes still in the kernel buffer — which is exactly the bug
// this method was added to fix.
func (s *Session) CloseSlave() error { return s.pts.Close() }

// Done returns a channel closed after Run returns.
func (s *Session) Done() <-chan struct{} { return s.done }

// ps1 builds the primary prompt. Honors $PS1 verbatim when set (no bash
// backslash-escape expansion — \u, \h, \w pass through literally). When
// unset, falls back to the common Unix default `user@host:cwd$ ` with
// `$HOME` collapsed to `~` and `$` switched to `#` for root.
func ps1(r *interp.Runner) string {
	if p := os.Getenv("PS1"); p != "" {
		return p
	}
	user := os.Getenv("USER")
	if user == "" {
		user = os.Getenv("USERNAME") // windows
	}
	if user == "" {
		user = "user"
	}
	host, _ := os.Hostname()
	if i := strings.Index(host, "."); i > 0 {
		host = host[:i] // bash `\h`: short hostname, drop FQDN suffix
	}
	if host == "" {
		host = "localhost"
	}
	cwd := r.Dir
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if cwd == home {
			cwd = "~"
		} else if strings.HasPrefix(cwd, home+string(os.PathSeparator)) {
			cwd = "~" + cwd[len(home):]
		}
	}
	sym := "$"
	if os.Getuid() == 0 { // -1 on Windows, never matches
		sym = "#"
	}
	return fmt.Sprintf("%s@%s:%s%s ", user, host, cwd, sym)
}

// ps2 is the continuation prompt for multi-line statements.
func ps2() string {
	if p := os.Getenv("PS2"); p != "" {
		return p
	}
	return "> "
}

// isExitStatus reports whether err is just a non-zero command exit code
// (interp.ExitStatus) rather than a fatal handler error. Used to decide
// whether to forward the error text to the user — bare exit codes are
// noise, real errors are not.
//
// Critically, this does NOT decide whether to end the session. That's
// the `runner.Exited()` check in Run — set only by the `exit` builtin
// or a fatal trap, never by `false` or a missing binary. Older code
// here conflated the two and ended the session on any non-zero status.
func isExitStatus(err error) bool {
	if err == nil {
		return false
	}
	var ec interp.ExitStatus
	return errorsAs(err, &ec)
}

// errorsAs is a tiny stand-in to avoid importing errors twice — we want a
// clean header in this file and the std-lib errors.As is the canonical way.
func errorsAs(err error, target any) bool { return stdErrorsAs(err, target) }
