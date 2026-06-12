package shell

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"mvdan.cc/sh/v3/interactive"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// RunLocal runs an interactive shell against the caller's stdio. No
// internal PTY is allocated — the caller's terminal is already a real
// TTY, so `interactive.Run` raws fd 0 directly via its `bindTTY`
// helper. Intended for `outpost shell`; the WebSocket/SSH paths still
// go through Session (which owns its own PTY pair).
//
// History file and the detached-job registry are shared with the
// matrix-shell path: a `nohup foo &` started here surfaces under
// `outpost jobs` just like one started over the matrix tunnel.
//
// Returns (exitCode, err). exitCode is 0 on clean exit (`exit` with
// no arg, Ctrl-D on an empty line), or N from `exit N`. err is
// non-nil only for setup failures or runner errors that aren't an
// exit-status carrier.
func RunLocal(ctx context.Context) (int, error) {
	runner, err := newLocalRunner(os.Stdin, os.Stdout, os.Stderr)
	if err != nil {
		return 1, err
	}
	err = interactive.Run(ctx, interactive.Options{
		Runner:            runner,
		Lang:              syntax.LangBash,
		Stdin:             os.Stdin,
		Stdout:            os.Stdout,
		Stderr:            os.Stderr,
		PS1:               func() string { return ps1(runner) },
		PS2:               func() string { return ps2() },
		Greeting:          "outpost shell (in-process bash; type 'exit' or Ctrl-D to quit)\n",
		HistoryFile:       outpostShellHistoryFile(),
		HistoryLimit:      1000,
		HistorySearchFold: true,
	})
	return exitCodeFromError(err)
}

// RunLocalCommand parses and runs `command` once against the supplied
// stdio, returning the runner's exit code. Same env construction +
// detached-job registry hook as RunLocal. Used by `outpost shell -c`.
//
// stdout/stderr default to os.Stdout/os.Stderr when nil; stdin
// defaults to os.Stdin. Returning (127, err) signals a parse failure.
func RunLocalCommand(ctx context.Context, command string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if stdin == nil {
		stdin = os.Stdin
	}
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	runner, err := newLocalRunner(stdin, stdout, stderr)
	if err != nil {
		return 1, err
	}
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return 127, fmt.Errorf("parse: %w", err)
	}
	return exitCodeFromError(runner.Run(ctx, file))
}

// newLocalRunner constructs an interp.Runner with the same env +
// detached-PID hook the matrix-shell Session uses, but wired to
// arbitrary stdio rather than a PTY pair.
func newLocalRunner(stdin io.Reader, stdout, stderr io.Writer) (*interp.Runner, error) {
	env := BuildEnvWith(nil)
	runner, err := interp.New(
		interp.StdIO(stdin, stdout, stderr),
		interp.Env(env),
		interp.ExecHandlers(CoreutilsExec), // PATH misses fall back to embedded coreutils (Windows!)
		interp.WithBgPidCallback(func(pid int) {
			_ = DefaultRegistry().Record(pid, "(detached)")
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("interp: %w", err)
	}
	return runner, nil
}

// exitCodeFromError unwraps an interp.ExitStatus into (N, nil); other
// errors become (1, err); nil → (0, nil).
func exitCodeFromError(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var ec interp.ExitStatus
	if stdErrorsAs(err, &ec) {
		return int(ec), nil
	}
	return 1, err
}
