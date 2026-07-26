package agent

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/qiangli/outpost/internal/agent/localsock"
	"github.com/qiangli/outpost/internal/agent/vknode"
)

// TestLivePodman_DetectAndPing is the on-hardware check behind the
// `vk-podman` cluster mode: it asserts that DetectPodman finds the local
// podman endpoint AND that the vknode libpod client can actually speak to
// it. Those are two different failures — detection can succeed while the
// dial fails, which is exactly how Windows broke (the candidate list had
// no Windows entry, and the client hardcoded a unix dial even though
// podman's endpoint there is `\\.\pipe\podman-<machine>`).
//
// Fake-libpod tests can't catch that class of bug: they hand the client a
// socket they created themselves. Only a real daemon on the real platform
// proves the endpoint is reachable.
//
// Gated on an env var because it needs a running podman, and skipping is
// the right behavior in unit-test runs. To run it:
//
//	OUTPOST_LIVE_PODMAN=1 go test ./internal/agent/ -run TestLivePodman -v
//
// Cross-platform, without a source tree on the target host:
//
//	GOOS=windows GOARCH=amd64 go test -c ./internal/agent -o agent.test.exe
//	# copy over, then on the host:
//	agent.test.exe -test.run TestLivePodman -test.v
func TestLivePodman_DetectAndPing(t *testing.T) {
	if os.Getenv("OUTPOST_LIVE_PODMAN") == "" {
		t.Skip("OUTPOST_LIVE_PODMAN not set — needs a running podman on this host")
	}

	bt := DetectPodman()
	t.Logf("DetectPodman: available=%v scheme=%q socket=%q", bt.Available, bt.Scheme, bt.Socket)
	// Log the whole candidate list on failure — on Windows the pipe name
	// embeds the operator-chosen machine name, so "what did we try" is
	// the first thing an operator needs to see.
	if !bt.Available {
		t.Logf("candidates tried: %v", podmanCandidates())
		t.Fatal("DetectPodman: no reachable podman endpoint")
	}
	if bt.Socket == "" {
		t.Fatal("DetectPodman: available but empty socket")
	}
	// The scheme must match the path shape, because it is what the app
	// registration and the libpod dial both key off.
	if want := localsock.Scheme(bt.Socket); bt.Scheme != want {
		t.Errorf("scheme %q disagrees with path shape %q", bt.Scheme, want)
	}

	// The real assertion: the libpod REST API answers over that endpoint.
	client, err := vknode.NewClient(bt.Socket)
	if err != nil {
		t.Fatalf("vknode.NewClient(%q): %v", bt.Socket, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.Ping(ctx); err != nil {
		t.Fatalf("libpod ping over %q (scheme %q): %v", bt.Socket, bt.Scheme, err)
	}
	t.Logf("libpod ping OK over %s endpoint %s", bt.Scheme, bt.Socket)

	// ListContainers exercises a real JSON round-trip, not just the
	// connect — a docker-compat endpoint that is NOT libpod (Docker
	// Desktop holding \\.\pipe\docker_engine) passes the ping but 404s
	// the /libpod/ tree, and that must surface here rather than at pod
	// scheduling time.
	items, err := client.ListContainers(ctx, true, nil)
	if err != nil {
		t.Fatalf("libpod list over %q: %v", bt.Socket, err)
	}
	t.Logf("libpod list OK: %d container(s) visible", len(items))
}
