package agent

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"
)

// BuiltinTarget describes one of the optional local-daemon proxies
// (podman, ollama). Available reports whether the daemon is reachable on
// the suggested socket/URL right now; the admin UI uses this to grey out
// toggles for daemons that aren't installed. Scheme is "unix" for socket
// targets and "http" for HTTP base-URL targets.
type BuiltinTarget struct {
	Name      string
	Scheme    string
	Socket    string // when Scheme == "unix"
	URL       string // when Scheme == "http" — full base URL, e.g. http://127.0.0.1:11434
	Available bool
}

// Builtin names — also the proxy slot names the admin UI surfaces and
// that get registered into the AppRegistry when enabled.
const (
	BuiltinPodman = "podman"
	BuiltinOllama = "ollama"
)

// DetectPodman probes the usual podman socket paths and returns a
// description suitable both for registering as an app and for grey-out
// rendering in the admin UI. The first reachable socket wins. When none
// are reachable, Socket is still populated with the first candidate so
// the UI can surface "tried <path>".
func DetectPodman() BuiltinTarget {
	cands := podmanCandidates()
	bt := BuiltinTarget{Name: BuiltinPodman, Scheme: "unix"}
	for _, p := range cands {
		if probeSocket(p, 200*time.Millisecond) {
			bt.Socket = p
			bt.Available = true
			return bt
		}
	}
	if len(cands) > 0 {
		bt.Socket = cands[0]
	}
	return bt
}

func podmanCandidates() []string {
	var paths []string
	uid := os.Getuid()
	switch runtime.GOOS {
	case "linux":
		// Rootless socket first — that's what most modern desktop installs
		// expose. Fall back to the system socket for root daemons.
		paths = append(paths, "/run/user/"+strconv.Itoa(uid)+"/podman/podman.sock")
		paths = append(paths, "/run/podman/podman.sock")
	case "darwin":
		// podman machine writes the socket somewhere under the user's data
		// dir; the exact subdir varies by machine name. Try the canonical
		// path first, then a couple common alternatives.
		home, _ := os.UserHomeDir()
		if home != "" {
			paths = append(paths,
				filepath.Join(home, ".local/share/containers/podman/machine/podman.sock"),
				filepath.Join(home, ".local/share/containers/podman/machine/podman-machine-default/podman.sock"),
			)
		}
		paths = append(paths, "/var/run/podman/podman.sock")
	}
	return paths
}

// DetectOllama probes the default Ollama HTTP endpoint. Ollama doesn't
// publish a health endpoint, so we just check that something HTTP-shaped
// is listening — the daemon answers any path with at least an HTTP
// status line.
func DetectOllama() BuiltinTarget {
	bt := BuiltinTarget{
		Name:   BuiltinOllama,
		Scheme: "http",
		URL:    "http://127.0.0.1:11434",
	}
	bt.Available = probeHTTP(bt.URL, 300*time.Millisecond)
	return bt
}

func probeSocket(path string, timeout time.Duration) bool {
	info, err := os.Stat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		return false
	}
	d := net.Dialer{Timeout: timeout}
	conn, err := d.Dial("unix", path)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func probeHTTP(url string, timeout time.Duration) bool {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/", nil)
	if err != nil {
		return false
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	// Any HTTP response (2xx/4xx/5xx) confirms a daemon is listening.
	// Network/dial errors are the only failure mode that matters here.
	return resp.StatusCode > 0
}

// BuiltinDetector caches DetectPodman/DetectOllama for a short TTL so
// repeated admin-UI calls don't probe the sockets on every request.
type BuiltinDetector struct {
	mu     sync.Mutex
	ttl    time.Duration
	now    func() time.Time
	cached map[string]builtinCacheEntry
}

type builtinCacheEntry struct {
	at    time.Time
	value BuiltinTarget
}

// NewBuiltinDetector returns a detector with the given probe-result TTL.
// Pass 0 to disable caching (mostly for tests).
func NewBuiltinDetector(ttl time.Duration) *BuiltinDetector {
	return &BuiltinDetector{
		ttl:    ttl,
		now:    time.Now,
		cached: map[string]builtinCacheEntry{},
	}
}

func (d *BuiltinDetector) lookup(name string, probe func() BuiltinTarget) BuiltinTarget {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ttl > 0 {
		if e, ok := d.cached[name]; ok && d.now().Sub(e.at) < d.ttl {
			return e.value
		}
	}
	v := probe()
	d.cached[name] = builtinCacheEntry{at: d.now(), value: v}
	return v
}

// Podman returns the cached or freshly-probed podman target.
func (d *BuiltinDetector) Podman() BuiltinTarget {
	return d.lookup(BuiltinPodman, DetectPodman)
}

// Ollama returns the cached or freshly-probed ollama target.
func (d *BuiltinDetector) Ollama() BuiltinTarget {
	return d.lookup(BuiltinOllama, DetectOllama)
}
