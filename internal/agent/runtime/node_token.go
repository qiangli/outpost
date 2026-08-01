package runtime

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// Reading the k3s node token out of the control plane this host runs.
//
// k3s writes the token to a file inside the server's data directory the moment
// the cluster is initialized. Until this existed the only way to get it was
// `podman exec <host>-control-plane cat …` typed by hand, which meant the
// documented way to join a peer's cluster began with a container command — the
// worker side of the same "built but unreachable" gap the join config had.
//
// It is read through `podman exec` rather than off the host filesystem because
// the data directory is a VOLUME, not a bind mount: on macOS and Windows it
// lives inside podman's VM, so there is no host path to read at all.

// ServerNodeTokenPath is where k3s writes the node token inside the
// control-plane container.
const ServerNodeTokenPath = "/var/lib/rancher/k3s/server/node-token"

// ServerNodeToken returns the k3s node token of the control plane this host
// hosts. The value is a CREDENTIAL — callers must print it to stdout on
// request and never log it.
//
// Errors distinguish the three ways this fails, because they need different
// fixes: no container engine, no control-plane container running, and a
// container that is up but has not initialized the cluster yet.
func ServerNodeToken(ctx context.Context, opts ServerOptions) (string, error) {
	if strings.TrimSpace(opts.AgentName) == "" {
		return "", errors.New("runtime: node token requires AgentName")
	}
	bin, err := pickPodmanBin(opts.PodmanBin)
	if err != nil {
		return "", err
	}
	name := ServerContainerName(opts.AgentName)

	// Output(), not CombinedOutput(): stderr must never be spliced into a
	// value the caller is about to print as a secret.
	out, err := exec.CommandContext(ctx, bin, "exec", name, "cat", ServerNodeTokenPath).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			detail := strings.TrimSpace(string(ee.Stderr))
			if detail == "" {
				detail = err.Error()
			}
			return "", fmt.Errorf("runtime: read node token from %s: %s", name, detail)
		}
		return "", fmt.Errorf("runtime: read node token from %s: %w", name, err)
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		return "", fmt.Errorf("runtime: %s has not written a node token yet (k3s may still be initializing)", name)
	}
	return token, nil
}
