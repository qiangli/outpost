// Package shell is the in-process bash interpreter (qiangli/sh / mvdan.cc/sh)
// wrapped in a PTY so xterm.js sees a real TTY: line discipline, echo,
// backspace, resize, and Ctrl-C all flow through the kernel TTY layer
// just as they would for a child `bash` process — except there is no
// child process.
//
// The interactive read-edit-execute loop lives in mvdan.cc/sh/v3/interactive
// (a fork-only package). That layer hosts the ergochat/readline integration
// — arrow-key history navigation, cursor movement, Ctrl-R reverse search —
// that the upstream parser.Interactive API does not provide. The PTY slave
// fd is what readline drives in raw mode while reading a line; for command
// execution between prompts the slave goes back to whatever termios the
// running command sets, so curses programs (vim, htop) see a real TTY.
package shell

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"mvdan.cc/sh/v3/interactive"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// ptyFile is one end of the session's terminal pair: a real PTY *os.File
// on unix, the pipe-backed emulation on Windows (see pty_windows.go).
type ptyFile interface {
	io.ReadWriteCloser
}

// virtualTTY is implemented by PTY emulations that have no kernel TTY
// behind them (the Windows pipe pair). When the slave end implements it,
// Session.Run tells readline to treat the stream as an already-raw
// terminal — the raw mode lives at the far end (SSH client / xterm.js)
// — and where to read the window size from.
type virtualTTY interface {
	WindowSize() (cols, rows int)
}

// stdinFiler is implemented by slave ends whose input side is a real
// *os.File. interp must receive that fd directly: for any other reader
// interp.StdIO spawns a stdin-draining copier goroutine (subprocesses
// can only inherit real fds) which would race readline for keystrokes.
type stdinFiler interface {
	StdinFile() *os.File
}

// Session is one interactive shell sitting between a tty pair and a runner.
// Caller writes to / reads from the master side of the PTY; the runner is
// hooked up to the slave side as stdin/stdout/stderr.
type Session struct {
	ptm    ptyFile // master (caller side)
	pts    ptyFile // slave (runner side)
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
	return newSessionFrom(ptm, pts, opts)
}

// newSessionFrom builds the Session on an already-open terminal pair.
// Split from NewSession so unix test runs can exercise the virtual
// (Windows) pair without a kernel PTY.
func newSessionFrom(ptm, pts ptyFile, opts SessionOptions) (*Session, error) {

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

	// interp stdin must be the slave's real fd when there is one — any
	// other reader makes interp.StdIO spawn a copier goroutine that
	// would steal keystrokes from readline (see stdinFiler).
	var stdin io.Reader = pts
	if sf, ok := pts.(stdinFiler); ok {
		stdin = sf.StdinFile()
	}
	runner, err := interp.New(
		interp.StdIO(stdin, pts, pts),
		interp.Env(env), // outpost process env + user-shell-style PATH extras (+ TERM if hinted)
		interp.ExecHandlers(CoreutilsExec), // PATH misses fall back to embedded coreutils (Windows!)
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
		_ = sessionSetSize(ptm, opts.Cols, opts.Rows)
	}
	return &Session{ptm: ptm, pts: pts, runner: runner, done: make(chan struct{})}, nil
}

// Master returns the master end. The caller pipes WebSocket bytes ↔ this.
func (s *Session) Master() io.ReadWriteCloser { return s.ptm }

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
func (s *Session) Resize(cols, rows uint16) error { return sessionSetSize(s.ptm, cols, rows) }

// sessionSetSize routes a resize to the right implementation: the
// virtual pair stores geometry in-process; a real PTY master takes the
// per-platform TIOCSWINSZ (pty_unix.go).
func sessionSetSize(master ptyFile, cols, rows uint16) error {
	if v, ok := master.(*vptyEnd); ok {
		v.pty.setSize(cols, rows)
		return nil
	}
	return setPTYSize(master, cols, rows)
}

// Run starts the interactive read-edit-execute loop, blocking until ctx is
// canceled, the user exits (the `exit` builtin or Ctrl-D on an empty line),
// or a fatal interp error.
//
// All line editing — arrow-key history navigation, cursor movement,
// backspace/Ctrl-W/Ctrl-U editing, Ctrl-R reverse search, history
// persistence — is delegated to mvdan.cc/sh/v3/interactive (which wraps
// ergochat/readline). The PTY slave fd is the TTY readline drives in raw
// mode; the swap back to cooked between prompts is what lets curses
// programs spawned by a stmt see a real /dev/ttysNN.
//
// Per-stmt cancellation: each parsed statement runs under a child context
// so a future signal-handling layer (Ctrl-C wiring on the PTY) can cancel
// just the current command without ending the session.
func (s *Session) Run(ctx context.Context) error {
	defer close(s.done)

	opts := interactive.Options{
		Runner:            s.runner,
		Lang:              syntax.LangBash,
		Stdin:             s.pts,
		Stdout:            s.pts,
		Stderr:            s.pts,
		PS1:               func() string { return ps1(s.runner) },
		PS2:               func() string { return ps2() },
		Greeting:          "Matrix shell (qiangli/sh in-process) — type `exit` or close the tab.\r\n",
		HistoryFile:       outpostShellHistoryFile(),
		HistoryLimit:      1000,
		HistorySearchFold: true,
		// PTY writes use \r\n line endings; the default error formatter would
		// emit only \n which renders as a stair-step in xterm.js.
		OnRunError: func(err error) {
			_, _ = io.WriteString(s.pts, err.Error()+"\r\n")
		},
	}
	if v, ok := s.pts.(virtualTTY); ok {
		// Windows pipe-backed pair: no kernel TTY to raw — the remote
		// terminal (SSH client / xterm.js) is already raw; readline does
		// echo + editing itself. Size comes from pty-req/window-change.
		opts.AssumeTTY = true
		opts.GetSize = v.WindowSize
	}
	return interactive.Run(ctx, opts)
}

// outpostShellHistoryFile returns the path the matrix-shell session uses to
// persist input lines across reconnects. Honors $OUTPOST_SHELL_HISTORY when
// set; otherwise defaults to <UserCacheDir>/outpost/shell_history. Returns
// "" (in-memory history only) when no cache dir is resolvable. The parent
// directory is created on first call — readline opens the file but does
// NOT create intermediate dirs.
func outpostShellHistoryFile() string {
	if v := os.Getenv("OUTPOST_SHELL_HISTORY"); v != "" {
		return v
	}
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		return ""
	}
	dir := filepath.Join(base, "outpost")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	return filepath.Join(dir, "shell_history")
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

// errorsAs is a tiny stand-in to avoid importing errors twice — we want a
// clean header in this file and the std-lib errors.As is the canonical way.
func errorsAs(err error, target any) bool { return stdErrorsAs(err, target) }
