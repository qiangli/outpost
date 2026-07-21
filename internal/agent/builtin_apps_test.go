package agent

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestOllamaBaseURL_DefaultWhenEnvUnset(t *testing.T) {
	t.Setenv("OLLAMA_HOST", "")
	if got := ollamaBaseURL(); got != "http://127.0.0.1:11434" {
		t.Errorf("ollamaBaseURL()=%q, want default", got)
	}
}

func TestOllamaBaseURL_HostPortForm(t *testing.T) {
	t.Setenv("OLLAMA_HOST", "127.0.0.1:8000")
	if got := ollamaBaseURL(); got != "http://127.0.0.1:8000" {
		t.Errorf("ollamaBaseURL()=%q, want http://127.0.0.1:8000", got)
	}
}

func TestOllamaBaseURL_FullURLForm(t *testing.T) {
	t.Setenv("OLLAMA_HOST", "https://ollama.example.com:9999/")
	if got := ollamaBaseURL(); got != "https://ollama.example.com:9999" {
		t.Errorf("ollamaBaseURL()=%q, want trimmed URL", got)
	}
}

func TestOllamaBaseURL_WhitespaceTolerant(t *testing.T) {
	t.Setenv("OLLAMA_HOST", "  ")
	if got := ollamaBaseURL(); got != "http://127.0.0.1:11434" {
		t.Errorf("whitespace-only OLLAMA_HOST should fall back to default, got %q", got)
	}
}

func TestUnixSocketPath(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"unix scheme", "unix:///run/podman/podman.sock", "/run/podman/podman.sock"},
		{"bare absolute path", "/run/podman/podman.sock", "/run/podman/podman.sock"},
		{"whitespace tolerant", "  unix:///tmp/p.sock  ", "/tmp/p.sock"},
		{"empty", "", ""},
		{"whitespace only", "   ", ""},
		// Remote daemons — a direct unix dial can't reach these, so they
		// must not be mistaken for a local socket.
		{"ssh remote", "ssh://core@127.0.0.1:62248/run/podman.sock", ""},
		{"tcp remote", "tcp://127.0.0.1:2375", ""},
		// A relative path is not a socket we can meaningfully dial.
		{"relative path", "podman.sock", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := unixSocketPath(tc.in); got != tc.want {
				t.Errorf("unixSocketPath(%q)=%q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// listenUnix creates a live unix socket so probeSocket() reports it
// reachable. Kept out of t.TempDir() because macOS caps sun_path at ~104
// bytes and the test-name-derived temp dirs run long.
func listenUnix(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "p")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Skipf("cannot listen on unix socket (%v)", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return path
}

func TestDetectPodman_ContainerHostWins(t *testing.T) {
	sock := listenUnix(t)
	t.Setenv("OUTPOST_PODMAN_SOCKET", "")
	t.Setenv("CONTAINER_HOST", "unix://"+sock)
	t.Setenv("DOCKER_HOST", "")

	bt := DetectPodman()
	if !bt.Available || bt.Socket != sock {
		t.Errorf("DetectPodman() = {Socket:%q Available:%v}, want reachable %q", bt.Socket, bt.Available, sock)
	}
}

func TestDetectPodman_DockerHostHonored(t *testing.T) {
	sock := listenUnix(t)
	t.Setenv("OUTPOST_PODMAN_SOCKET", "")
	t.Setenv("CONTAINER_HOST", "")
	t.Setenv("DOCKER_HOST", "unix://"+sock)

	bt := DetectPodman()
	if !bt.Available || bt.Socket != sock {
		t.Errorf("DetectPodman() = {Socket:%q Available:%v}, want reachable %q", bt.Socket, bt.Available, sock)
	}
}

// A set-but-dead endpoint must not mask a working podman — $DOCKER_HOST in
// particular is often left pointing at a stale socket.
func TestDetectPodman_UnreachableEnvFallsThrough(t *testing.T) {
	dead := filepath.Join(t.TempDir(), "nope.sock")
	t.Setenv("OUTPOST_PODMAN_SOCKET", "")
	t.Setenv("CONTAINER_HOST", "unix://"+dead)
	t.Setenv("DOCKER_HOST", "")

	bt := DetectPodman()
	if bt.Available {
		// A real podman on the dev box answered via autodetection — which
		// is exactly the fall-through we wanted.
		if bt.Socket == dead {
			t.Errorf("reported the dead env socket as available: %q", dead)
		}
		return
	}
	// Nothing reachable anywhere: the env endpoint is the most useful
	// "tried" hint to show the operator.
	if bt.Socket != dead {
		t.Errorf("Socket=%q, want the tried env endpoint %q", bt.Socket, dead)
	}
}

func TestPodmanCandidates_DarwinPrefersMachineAPISocket(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("darwin-only candidate layout")
	}
	want := filepath.Join(os.TempDir(), "podman", "*-api.sock")
	cands := podmanCandidates()
	i := slices.Index(cands, want)
	if i < 0 {
		t.Fatalf("podman machine API socket glob %q missing from candidates %v", want, cands)
	}
	// Must precede the older data-dir layouts, which current podman
	// releases no longer write to.
	for j, c := range cands {
		if strings.Contains(c, ".local/share/containers") && j < i {
			t.Errorf("stale data-dir candidate %q ordered before %q", c, want)
		}
	}
}
