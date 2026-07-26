package localsock

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLivePodmanEndpoint is the on-hardware proof for this package: it
// finds the real local podman endpoints and speaks libpod over them.
//
// It exists as a SEPARATE, small test binary on purpose. The equivalent
// test in package agent (TestLivePodman_DetectAndPing) links the whole
// daemon and cross-compiles to ~140 MB, which cannot be delivered to a
// remote Windows host over the cloudbox relay — the data-plane egress
// block truncates it. This package is stdlib + winio, so `go test -c`
// yields a few MB that copies fine.
//
// What it proves on Windows, none of which a fake-socket test can:
//   - ListPipes enumerates the `\\.\pipe\` namespace (there is no glob
//     for it), so an operator-chosen machine name is found without config
//   - Probe works for a named pipe AND for the Windows AF_UNIX socket
//     podman also publishes (where there is no os.ModeSocket bit)
//   - the DialFunc that vknode.NewClient installs actually reaches libpod
//
// Run it with:
//
//	OUTPOST_LIVE_PODMAN=1 go test ./internal/agent/localsock -run TestLive -v
//
// or, on a host with no source tree:
//
//	GOOS=windows GOARCH=amd64 go test -c ./internal/agent/localsock -o ls.test.exe
//	ls.test.exe -test.run TestLive -test.v
func TestLivePodmanEndpoint(t *testing.T) {
	if os.Getenv("OUTPOST_LIVE_PODMAN") == "" {
		t.Skip("OUTPOST_LIVE_PODMAN not set — needs a running podman on this host")
	}

	var cands []string
	// Windows: the per-machine pipe, discovered by enumeration.
	pipes := ListPipes("podman-")
	t.Logf("ListPipes(%q) → %v", "podman-", pipes)
	cands = append(cands, pipes...)
	// Every platform: podman machine's AF_UNIX socket, whose name embeds
	// the machine name, hence the glob.
	if tmp := os.TempDir(); tmp != "" {
		matches, _ := filepath.Glob(filepath.Join(tmp, "podman", "*-api.sock"))
		t.Logf("api.sock glob under %s → %v", tmp, matches)
		cands = append(cands, matches...)
	}
	// The docker-compat pipe, last (it may be held by Docker Desktop,
	// which answers a connect but not the /libpod/ tree).
	cands = append(cands, PipePath("docker_engine"))

	if len(cands) == 0 {
		t.Fatal("no podman endpoint candidates found on this host")
	}

	var reached []string
	for _, c := range cands {
		ok := Probe(c, 2*time.Second)
		t.Logf("Probe(%q) scheme=%s → %v", c, Scheme(c), ok)
		if !ok {
			continue
		}
		if err := libpodPing(c); err != nil {
			t.Logf("  libpod ping over %q FAILED: %v", c, err)
			continue
		}
		t.Logf("  libpod ping OK over %q", c)
		reached = append(reached, c)
	}

	if len(reached) == 0 {
		t.Fatalf("no candidate served the libpod API; tried %v", cands)
	}
	t.Logf("PASS: %d/%d endpoint(s) served libpod: %v", len(reached), len(cands), reached)
}

// libpodPing issues the cheapest real libpod request over the given
// endpoint using the same DialFunc vknode.NewClient installs. A non-200
// is a failure: it is how a docker-compat-only endpoint (dockerd, not
// libpod) reveals itself, since it accepts the connection and then 404s.
func libpodPing(socket string) error {
	client := &http.Client{
		Transport: &http.Transport{DialContext: DialFunc(socket)},
		Timeout:   10 * time.Second,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://podman/v5.0.0/libpod/_ping", nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	if resp.StatusCode != http.StatusOK {
		return &pingError{status: resp.StatusCode, body: string(body)}
	}
	return nil
}

type pingError struct {
	status int
	body   string
}

func (e *pingError) Error() string {
	return "libpod _ping returned HTTP " + http.StatusText(e.status) + " (" + itoa(e.status) + "): " + e.body
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
