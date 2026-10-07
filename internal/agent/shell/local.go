package shell

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/qiangli/outpost/internal/bashypath"
)

// RunLocal gives Bashy the caller's existing terminal without allocating a PTY.
func RunLocal(ctx context.Context) (int, error) {
	return runLocal(ctx, []string{"-i"}, nil, nil, nil, nil)
}

func RunLocalCommand(ctx context.Context, command string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	return RunCommand(ctx, command, stdin, stdout, stderr, nil)
}

// RunCommand is the non-PTY SSH and local execution path. It preserves the
// child's exit status and applies per-session overrides such as SSH_AUTH_SOCK.
func RunCommand(ctx context.Context, command string, stdin io.Reader, stdout, stderr io.Writer, env map[string]string) (int, error) {
	return runLocal(ctx, []string{"-c", command}, stdin, stdout, stderr, env)
}
func runLocal(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, env map[string]string) (int, error) {
	path, _, err := bashypath.Find(nil)
	if err != nil {
		return 1, err
	}
	if stdin == nil {
		stdin = os.Stdin
	}
	if stdout == nil {
		stdout = os.Stdout
	}
	if stderr == nil {
		stderr = os.Stderr
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	cmd.Env = BuildEnvWith(env)
	configureCommand(cmd, len(args) > 0 && args[0] == "-c")
	// A detached grandchild must not hold this request's output pipes forever.
	cmd.WaitDelay = 2 * time.Second
	return exitCodeFromError(cmd.Run())
}
func exitCodeFromError(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if exit.ExitCode() < 0 {
			return 128, nil
		}
		return exit.ExitCode(), nil
	}
	return 1, err
}
