// Package localsock is the one place outpost decides how to reach a
// local daemon endpoint that is not a TCP port: a unix-domain socket or
// a Windows named pipe.
//
// It exists because three independent callers each need the same
// decision and had each hardcoded `net.Dial("unix", …)`: the app-proxy
// dialer (agent.dialSocket), the vknode libpod client, and the sandbox
// pre-warmer. A unix-only dial is wrong on Windows, where podman
// machine publishes its libpod API on `\\.\pipe\podman-<machine>` (see
// podman's pkg/machine.launchWinProxy) — so `vk-podman` could never
// reach a Windows podman at all.
//
// Two rules are load-bearing:
//
//   - The SCHEME IS INFERRED FROM THE PATH, never guessed by the
//     caller. `\\.\pipe\x` is a pipe; everything else is a unix socket.
//     That keeps a single string (a config value, an env var, a probe
//     result) sufficient to reach the daemon.
//   - AF_UNIX EXISTS ON WINDOWS (10 1803+) and Go supports it, so the
//     unix branch is NOT unix-only. podman publishes both a pipe and an
//     AF_UNIX socket (`%TEMP%\podman\<machine>-api.sock`) on Windows;
//     both must work. What differs is stat: Windows reports an AF_UNIX
//     socket as a REGULAR FILE, with no os.ModeSocket bit — hence the
//     platform-split statOK in pipe_{windows,other}.go.
package localsock

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"time"
)

// Scheme names, matching the values conf.AppConfig.Scheme already uses
// for socket-backed apps so a detected endpoint can be handed straight
// to RegisterFromConfig.
const (
	SchemeUnix  = "unix"
	SchemeNPipe = "npipe"
)

// PipePrefix is the Win32 named-pipe namespace root. Declared here (not
// behind a build tag) because building a candidate path is
// platform-independent — only dialing one is not.
const PipePrefix = `\\.\pipe\`

// pipePrefixSlash is the forward-slash spelling Win32 accepts and that
// podman prints in its `npipe:////./pipe/<name>` URLs.
const pipePrefixSlash = "//./pipe/"

// PipePath returns the dialable path for a named pipe called name.
func PipePath(name string) string {
	return PipePrefix + name
}

// IsPipe reports whether path names a Windows named pipe. Accepts both
// the backslash spelling (`\\.\pipe\x`) and the forward-slash spelling
// (`//./pipe/x`) Win32 also honors; matching is case-insensitive
// because the pipe namespace is.
func IsPipe(path string) bool {
	s := strings.ToLower(strings.TrimSpace(path))
	return strings.HasPrefix(s, strings.ToLower(PipePrefix)) ||
		strings.HasPrefix(s, pipePrefixSlash)
}

// Scheme returns the conf-level scheme name for path.
func Scheme(path string) string {
	if IsPipe(path) {
		return SchemeNPipe
	}
	return SchemeUnix
}

// Normalize extracts a dialable local-socket path from a daemon-endpoint
// value of the kind $CONTAINER_HOST / $DOCKER_HOST carry, or from a bare
// path. It returns "" for endpoints a direct local dial cannot reach —
// ssh:// and tcp:// name a remote daemon, so silently treating them as
// paths would produce a confusing ENOENT instead of "not local".
//
// Accepted forms:
//
//	unix:///run/podman/podman.sock   → /run/podman/podman.sock
//	unix://C:\…\bashy-api.sock       → C:\…\bashy-api.sock   (bashy sets this)
//	npipe:////./pipe/docker_engine   → \\.\pipe\docker_engine (podman prints this)
//	//./pipe/podman-bashy            → \\.\pipe\podman-bashy
//	\\.\pipe\podman-bashy            → unchanged
//	/run/podman/podman.sock          → unchanged
//	C:\…\bashy-api.sock              → unchanged
func Normalize(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	// A pipe spelling may arrive with or without a scheme; check before
	// the generic "://" reject so npipe:// isn't discarded as remote.
	if rest, ok := cutSchemePrefix(s, "npipe://"); ok {
		return canonicalPipe(rest)
	}
	if rest, ok := cutSchemePrefix(s, "unix://"); ok {
		// An explicit unix:// scheme states the intent, so the remainder
		// is taken verbatim — no absoluteness check. That matters for
		// cross-platform values: bashy exports
		// `unix://C:\…\<machine>-api.sock` on Windows, and a POSIX host
		// re-validating that path would reject it as relative.
		return strings.TrimSpace(rest)
	}
	if strings.Contains(s, "://") {
		// ssh://, tcp://, http:// — a remote daemon, not a local socket.
		return ""
	}
	if IsPipe(s) {
		return canonicalPipe(s)
	}
	// A bare path has no scheme to vouch for it, so require it to be
	// absolute — that is what separates a real endpoint from junk.
	// filepath.IsAbs is platform-aware (it accepts `C:\x` only on
	// Windows), so test the POSIX form explicitly as well.
	if strings.HasPrefix(s, "/") || filepath.IsAbs(s) {
		return s
	}
	return ""
}

// cutSchemePrefix strips a case-insensitive scheme prefix.
func cutSchemePrefix(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return "", false
	}
	return s[len(prefix):], true
}

// canonicalPipe rewrites any accepted pipe spelling into the backslash
// form winio wants (`\\.\pipe\<name>`).
//
// It works by reducing the input to the bare pipe NAME and re-prefixing,
// because the same pipe is spelled several ways in the wild and a
// prefix-by-prefix approach kept mis-parsing one of them:
//
//	\\.\pipe\<name>    already canonical (podman machine inspect)
//	//./pipe/<name>    podman's npipe:////./pipe/<name>, post scheme-cut
//	./pipe/<name>      npipe://./pipe/<name>, where "." is the URL host
//	<name>             a bare pipe name
func canonicalPipe(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(strings.ToLower(s), strings.ToLower(PipePrefix)) {
		return s
	}
	t := strings.ReplaceAll(s, `\`, "/")
	t = strings.TrimLeft(t, "/")
	t = strings.TrimPrefix(t, "./")
	if rest, ok := cutSchemePrefix(t, "pipe/"); ok {
		t = rest
	}
	return PipePath(t)
}

// Dial connects to the local socket at path, inferring the transport
// from the path shape. The returned conn is suitable for an
// http.Transport.DialContext.
func Dial(ctx context.Context, path string) (net.Conn, error) {
	p := strings.TrimSpace(path)
	if p == "" {
		return nil, fmt.Errorf("localsock: empty socket path")
	}
	if IsPipe(p) {
		return dialPipe(ctx, canonicalPipe(p))
	}
	var d net.Dialer
	return d.DialContext(ctx, "unix", p)
}

// DialFunc returns a DialContext closure bound to path, for dropping
// straight into an http.Transport. The network/addr arguments the
// transport passes are ignored — the endpoint is the path.
func DialFunc(path string) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, _, _ string) (net.Conn, error) {
		return Dial(ctx, path)
	}
}

// Probe reports whether something is listening at path. It is the
// availability check behind the admin UI's greyed-out daemon toggles, so
// it must be cheap and must never block longer than timeout.
//
// A successful dial is the only positive signal: on Windows there is no
// socket file mode to inspect, and a stale AF_UNIX socket file outlives
// the daemon that created it on every platform.
func Probe(path string, timeout time.Duration) bool {
	p := Normalize(path)
	if p == "" {
		return false
	}
	if !statOK(p) {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	conn, err := Dial(ctx, p)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
