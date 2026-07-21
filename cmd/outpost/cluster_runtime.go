package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/qiangli/outpost/internal/agent"
)

// ensurePodmanRuntime resolves a reachable podman socket, starting the podman
// machine when one exists but isn't running.
//
// outpost owns the runtime's lifecycle because cluster mode depends on it: the
// OS boot service starts outpost before any login-time container runtime, so
// "podman isn't up yet" is the normal state at boot rather than an error. The
// alternative — a separate login job that starts the machine and then pokes
// outpost to re-probe — makes correctness depend on the ordering of two
// independent launchers, which is exactly the race this avoids.
//
// Deliberately does NOT run `podman machine init` when no machine exists: that
// downloads a VM image and provisions a large disk, which a boot service should
// never do implicitly. Returns an actionable error instead.
func ensurePodmanRuntime(ctx context.Context) (string, error) {
	if bt := agent.DetectPodman(); bt.Available && bt.Socket != "" {
		return bt.Socket, nil
	}
	// Linux runs podman natively — there is no VM to start, so a missing
	// socket means the service itself isn't running and starting a
	// "machine" would be meaningless.
	if runtime.GOOS == "linux" {
		return "", errors.New("podman socket not detected (is the podman service running?)")
	}

	podman, err := podmanCommand(ctx)
	if err != nil {
		return "", err
	}
	name, err := podmanMachineToStart(ctx, podman)
	if err != nil {
		return "", err
	}

	slog.Info("cluster mode: starting podman machine", "machine", name)
	startCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	// Idempotent: a concurrent start (or a machine that came up between the
	// list and now) reports "already running", which is success for us.
	if out, err := runPodman(startCtx, podman, "machine", "start", name); err != nil &&
		!strings.Contains(strings.ToLower(out), "already running") {
		return "", fmt.Errorf("podman machine start %s: %w (%s)", name, err, firstLine(out))
	}

	bt := agent.DetectPodman()
	if !bt.Available || bt.Socket == "" {
		return "", fmt.Errorf("podman machine %q started but no socket became reachable (tried %s)", name, bt.Socket)
	}
	slog.Info("cluster mode: podman runtime ready", "machine", name, "socket", bt.Socket)
	return bt.Socket, nil
}

// podmanCommand resolves how to invoke podman, returning the argv prefix. A
// podman on PATH is operator-owned and wins; otherwise fall back to the bashy
// binary outpost already manages, which ships podman as a subcommand.
func podmanCommand(ctx context.Context) ([]string, error) {
	if p, err := exec.LookPath("podman"); err == nil {
		return []string{p}, nil
	}
	if p, err := bashyResolver.Path(ctx); err == nil && p != "" {
		return []string{p, "podman"}, nil
	}
	return nil, errors.New("no podman available (not on PATH, and no bashy to provide it)")
}

// podmanMachine is the subset of `podman machine list --format json` we need.
type podmanMachine struct {
	Name    string `json:"Name"`
	Default bool   `json:"Default"`
	Running bool   `json:"Running"`
}

// podmanMachineToStart picks which machine to bring up: the default one (what
// every other podman command targets), falling back to the only/first entry.
func podmanMachineToStart(ctx context.Context, podman []string) (string, error) {
	listCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := runPodman(listCtx, podman, "machine", "list", "--format", "json")
	if err != nil {
		return "", fmt.Errorf("podman machine list: %w (%s)", err, firstLine(out))
	}
	var machines []podmanMachine
	if err := json.Unmarshal([]byte(out), &machines); err != nil {
		return "", fmt.Errorf("podman machine list: parse output: %w", err)
	}
	if len(machines) == 0 {
		return "", errors.New("no podman machine exists — run `podman machine init` once to provision one")
	}
	for _, m := range machines {
		if m.Default {
			return m.Name, nil
		}
	}
	return machines[0].Name, nil
}

func runPodman(ctx context.Context, podman []string, args ...string) (string, error) {
	argv := append(append([]string{}, podman...), args...)
	out, err := exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput()
	return string(out), err
}

// firstLine trims a command's output to its first line — enough to identify a
// failure without pasting a whole podman banner into the log.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}
