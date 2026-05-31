package agent

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
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
	bt := BuiltinTarget{Name: BuiltinPodman, Scheme: "unix"}
	// Operator override: $OUTPOST_PODMAN_SOCKET wins over autodetection.
	// Accepts either a literal path or a shell-style glob (any of *?[);
	// when the glob expands to multiple matches the newest by mtime
	// wins. Lets ycode-style sockets — `~/.agents/ycode/podman-<pid>.sock`,
	// where the PID changes on every ycode restart — be configured once
	// without baking the pattern into the candidate list.
	if env := strings.TrimSpace(os.Getenv("OUTPOST_PODMAN_SOCKET")); env != "" {
		sock := env
		if strings.ContainsAny(env, "*?[") {
			sock = newestGlobMatch(env)
		}
		bt.Socket = sock
		if sock != "" && probeSocket(sock, 200*time.Millisecond) {
			bt.Available = true
		}
		return bt
	}
	cands := podmanCandidates()
	for _, p := range cands {
		// Each candidate may be a literal path OR a shell-style glob.
		// Globs let us track sockets whose filename embeds a process
		// ID that rotates — notably ycode's per-pid sockets at
		// ~/.agents/ycode/podman-<pid>.sock. newest-by-mtime wins
		// when multiple match.
		sock := p
		if strings.ContainsAny(p, "*?[") {
			sock = newestGlobMatch(p)
			if sock == "" {
				continue
			}
		}
		if probeSocket(sock, 200*time.Millisecond) {
			bt.Socket = sock
			bt.Available = true
			return bt
		}
	}
	if len(cands) > 0 {
		bt.Socket = cands[0]
	}
	return bt
}

// newestGlobMatch expands a shell-style glob and returns the path
// with the newest mtime, or "" when nothing matches. Used by the
// OUTPOST_PODMAN_SOCKET override to track ycode-style sockets whose
// filename embeds a PID that changes per ycode restart.
func newestGlobMatch(pattern string) string {
	matches, _ := filepath.Glob(pattern)
	var newest string
	var newestMtime time.Time
	for _, m := range matches {
		info, err := os.Stat(m)
		if err != nil {
			continue
		}
		if info.ModTime().After(newestMtime) {
			newestMtime = info.ModTime()
			newest = m
		}
	}
	return newest
}

func podmanCandidates() []string {
	var paths []string
	uid := os.Getuid()
	home, _ := os.UserHomeDir()
	// ycode-managed sockets are first-class on every platform we
	// support — when the operator runs ycode, this is the socket
	// outpost should prefer because it lines up with ycode's
	// container management. The PID changes per ycode restart, so
	// the path is a glob expanded via newest-mtime in DetectPodman.
	if home != "" {
		paths = append(paths, filepath.Join(home, ".agents", "ycode", "podman-*.sock"))
	}
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

// DetectOllama probes the local Ollama HTTP endpoint. Ollama doesn't
// publish a health endpoint, so we just check that something HTTP-shaped
// is listening — the daemon answers any path with at least an HTTP
// status line.
//
// Honors $OLLAMA_HOST when set (Ollama's own env-var contract — users
// who run the daemon on a non-default port set this and expect every
// tool in the ecosystem to follow). Accepts both bare "host:port" and
// full "http(s)://host:port" forms. Falls back to the default loopback
// URL when unset.
func DetectOllama() BuiltinTarget {
	bt := BuiltinTarget{
		Name:   BuiltinOllama,
		Scheme: "http",
		URL:    ollamaBaseURL(),
	}
	bt.Available = probeHTTP(bt.URL, 300*time.Millisecond)
	return bt
}

// ollamaBaseURL resolves the Ollama daemon's HTTP base URL from the
// environment. Exported via DetectOllama; tests can poke $OLLAMA_HOST
// directly. Defaults to http://127.0.0.1:11434 — Ollama's own default.
func ollamaBaseURL() string {
	h := strings.TrimSpace(os.Getenv("OLLAMA_HOST"))
	if h == "" {
		return "http://127.0.0.1:11434"
	}
	if strings.Contains(h, "://") {
		return strings.TrimRight(h, "/")
	}
	return "http://" + h
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
