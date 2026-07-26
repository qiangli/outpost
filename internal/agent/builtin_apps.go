package agent

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/outpost/internal/agent/localsock"
)

// BuiltinTarget describes one of the optional local-daemon proxies
// (podman, ollama). Available reports whether the daemon is reachable on
// the suggested socket/URL right now; the admin UI uses this to grey out
// toggles for daemons that aren't installed. Scheme is "unix" or "npipe"
// for socket targets and "http" for HTTP base-URL targets.
//
// Scheme is AUTHORITATIVE for socket targets and must be threaded into
// the AppConfig the caller registers — a Windows podman is reached over
// a named pipe, so a caller that hardcodes "unix" cannot talk to it.
type BuiltinTarget struct {
	Name      string
	Scheme    string
	Socket    string // when Scheme == "unix" or "npipe"
	URL       string // when Scheme == "http" — full base URL, e.g. http://127.0.0.1:11434
	Available bool
}

// setSocket records a detected socket path together with the transport
// it implies. Keeping the two assignments in one place is what stops a
// new detection branch from leaving Scheme at its default.
func (bt *BuiltinTarget) setSocket(path string) {
	bt.Socket = path
	if path != "" {
		bt.Scheme = localsock.Scheme(path)
	}
}

// Builtin names — also the proxy slot names the admin UI surfaces and
// that get registered into the AppRegistry when enabled.
const (
	BuiltinPodman = "podman"
	BuiltinOllama = "ollama"
	// BuiltinSandbox is the filtered container-sandbox mount. It speaks to
	// the same podman socket DetectPodman() finds (so availability is
	// gated on podman being installed), but registers a SEPARATE app whose
	// proxy is wrapped by the sandbox filter — distinct from the raw,
	// admin-only BuiltinPodman passthrough.
	BuiltinSandbox = "sandbox"
	// BuiltinFiles is the embedded File Browser mount — an in-process HTTP
	// handler (not an external daemon), the GUI sibling of /shell + /ssh
	// for remote view/download. Registered as a normal "http" app so it
	// flows through the existing per-app gate.
	BuiltinFiles = "files"
)

// DetectPodman probes the usual podman socket paths and returns a
// description suitable both for registering as an app and for grey-out
// rendering in the admin UI. The first reachable socket wins. When none
// are reachable, Socket is still populated with the first candidate so
// the UI can surface "tried <path>".
func DetectPodman() BuiltinTarget {
	bt := BuiltinTarget{Name: BuiltinPodman, Scheme: localsock.SchemeUnix}
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
		bt.setSocket(sock)
		if sock != "" && probeSocket(sock, 200*time.Millisecond) {
			bt.Available = true
		}
		return bt
	}
	// Ecosystem contract: podman's own $CONTAINER_HOST — and $DOCKER_HOST,
	// which podman honors for docker-compat — name the daemon endpoint.
	// Mirrors what DetectOllama does with $OLLAMA_HOST: a user who moved
	// the socket expects every tool to follow. Only unix-socket forms are
	// usable here; ssh:// / tcp:// point at a remote daemon this
	// direct-dial path can't serve. A set-but-unreachable value falls
	// through to autodetection rather than masking a working podman —
	// $DOCKER_HOST in particular is often left pointing at a stale or
	// docker-owned socket.
	var envSock string
	for _, key := range []string{"CONTAINER_HOST", "DOCKER_HOST"} {
		sock := localSocketPath(os.Getenv(key))
		if sock == "" {
			continue
		}
		if envSock == "" {
			envSock = sock
		}
		if probeSocket(sock, 200*time.Millisecond) {
			bt.setSocket(sock)
			bt.Available = true
			return bt
		}
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
			bt.setSocket(sock)
			bt.Available = true
			return bt
		}
	}
	// Nothing reachable — populate Socket with the best "tried" hint so the
	// admin UI can say what was attempted. An explicitly configured
	// endpoint is a more useful answer than the first autodetect candidate.
	if envSock != "" {
		bt.setSocket(envSock)
	} else if len(cands) > 0 {
		bt.setSocket(cands[0])
	}
	return bt
}

// localSocketPath extracts a dialable local-socket path from a
// daemon-endpoint URL of the form used by $CONTAINER_HOST / $DOCKER_HOST.
// Accepts "unix:///run/podman.sock", "npipe:////./pipe/docker_engine"
// (the form podman prints on Windows), and bare absolute paths; returns
// "" for remote schemes (ssh://, tcp://), which a direct local dial
// can't reach.
func localSocketPath(raw string) string {
	return localsock.Normalize(raw)
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
		// podman machine advertises its API socket under the per-user
		// TMPDIR as <machine>-api.sock — the path `podman machine start`
		// prints and the one it tells you to point $DOCKER_HOST at. The
		// machine name is operator-chosen (not always
		// "podman-machine-default"), so this is a glob; newest-by-mtime
		// wins when several machines have run. Listed first because it's
		// where a current podman actually puts the socket — the data-dir
		// paths below are older layouts kept as fallbacks.
		if tmp := os.TempDir(); tmp != "" {
			paths = append(paths, filepath.Join(tmp, "podman", "*-api.sock"))
		}
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
	case "windows":
		// `podman machine start` launches win-sshproxy.exe, which
		// publishes the SAME libpod API on three endpoints (see
		// launchWinProxy in podman's pkg/machine/machine_windows.go):
		// the per-machine pipe `\\.\pipe\podman-<machine>`, the
		// docker-compat pipe `\\.\pipe\docker_engine` when that global
		// name is free, and an AF_UNIX socket at
		// %TEMP%\podman\<machine>-api.sock.
		//
		// The machine name is operator-chosen (bashy names its isolated
		// VM "bashy"; upstream's default is "podman-machine-default"),
		// so we ENUMERATE the pipe namespace for the `podman-` prefix
		// rather than guessing names — filepath.Glob cannot walk
		// `\\.\pipe\`, but a directory read can.
		paths = append(paths, localsock.ListPipes("podman-")...)
		// Same layout as the darwin case above: a machine-name glob
		// resolved newest-mtime-first by DetectPodman. AF_UNIX works on
		// Windows 10 1803+, so this is a real candidate, not a fallback.
		if tmp := os.TempDir(); tmp != "" {
			paths = append(paths, filepath.Join(tmp, "podman", "*-api.sock"))
		}
		// Last: the global docker-compat pipe. Deliberately after the
		// podman-specific endpoints because this name may instead be
		// held by Docker Desktop, whose dockerd does NOT serve the
		// /v5.0.0/libpod/* tree vknode speaks — it would pass the
		// connect probe and then 404 every request.
		paths = append(paths, localsock.PipePath("docker_engine"))
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

// probeSocket reports whether a daemon is listening on the local socket
// at path. Handles both AF_UNIX sockets and Windows named pipes — see
// localsock, which owns the "which transport is this path" decision.
func probeSocket(path string, timeout time.Duration) bool {
	return localsock.Probe(path, timeout)
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
