package localsock

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestNormalize pins the endpoint-string parsing on EVERY platform, not
// just Windows. The Windows dial path can't be exercised from a unix CI
// leg, so the shape decisions ("is this a pipe", "which path do I dial")
// are what the tests have to hold — a regression here is what silently
// made vk-podman unreachable on Windows before.
func TestNormalize(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"blank", "   ", ""},

		// unix forms
		{"unix scheme", "unix:///run/podman/podman.sock", "/run/podman/podman.sock"},
		{"bare posix path", "/run/user/1000/podman/podman.sock", "/run/user/1000/podman/podman.sock"},
		{"trims space", "  /run/podman.sock  ", "/run/podman.sock"},

		// The endpoint bashy's `podman` pass-through exports on Windows:
		// a unix:// URL wrapping a drive-letter path. filepath.IsAbs
		// handles the drive form only on Windows, hence the explicit
		// POSIX check in Normalize.
		{"unix scheme with windows path", `unix://C:\Users\me\AppData\Local\Temp\podman\bashy-api.sock`,
			`C:\Users\me\AppData\Local\Temp\podman\bashy-api.sock`},

		// npipe forms. podman prints the four-slash spelling verbatim
		// ("API forwarding listening on: npipe:////./pipe/docker_engine"),
		// so that exact string must round-trip to a dialable pipe path.
		{"npipe podman spelling", "npipe:////./pipe/docker_engine", `\\.\pipe\docker_engine`},
		{"npipe two slash", "npipe://./pipe/podman-bashy", `\\.\pipe\podman-bashy`},
		{"bare forward slash pipe", "//./pipe/podman-bashy", `\\.\pipe\podman-bashy`},
		{"bare backslash pipe", `\\.\pipe\podman-bashy`, `\\.\pipe\podman-bashy`},
		{"pipe scheme case insensitive", "NPIPE:////./pipe/podman-bashy", `\\.\pipe\podman-bashy`},

		// Remote endpoints are NOT local sockets. Returning "" here is
		// load-bearing: podman on Windows lists its machine as
		// ssh://user@127.0.0.1:<port>/run/podman/podman.sock, and treating
		// that as a path would produce a baffling ENOENT instead of
		// falling through to the next candidate.
		{"ssh rejected", "ssh://user@127.0.0.1:49962/run/podman/podman.sock", ""},
		{"tcp rejected", "tcp://127.0.0.1:2375", ""},
		{"http rejected", "http://127.0.0.1:2375", ""},
		{"relative rejected", "podman.sock", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Normalize(tc.in); got != tc.want {
				t.Errorf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestIsPipeAndScheme(t *testing.T) {
	pipes := []string{
		`\\.\pipe\docker_engine`,
		`\\.\pipe\podman-bashy`,
		`//./pipe/podman-bashy`,
		`\\.\PIPE\podman-bashy`,
	}
	for _, p := range pipes {
		if !IsPipe(p) {
			t.Errorf("IsPipe(%q) = false, want true", p)
		}
		if got := Scheme(p); got != SchemeNPipe {
			t.Errorf("Scheme(%q) = %q, want %q", p, got, SchemeNPipe)
		}
	}
	socks := []string{
		"/run/podman/podman.sock",
		`C:\Users\me\AppData\Local\Temp\podman\bashy-api.sock`,
		"",
	}
	for _, s := range socks {
		if IsPipe(s) {
			t.Errorf("IsPipe(%q) = true, want false", s)
		}
		if got := Scheme(s); got != SchemeUnix {
			t.Errorf("Scheme(%q) = %q, want %q", s, got, SchemeUnix)
		}
	}
}

func TestPipePath(t *testing.T) {
	if got, want := PipePath("podman-bashy"), `\\.\pipe\podman-bashy`; got != want {
		t.Errorf("PipePath = %q, want %q", got, want)
	}
}

// TestDialPipeOffWindows asserts the loud-failure contract: a pipe path
// on a non-Windows build must return a clear error rather than be
// silently reinterpreted as a unix socket.
func TestDialPipeOffWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows dials pipes for real")
	}
	if _, err := Dial(t.Context(), `\\.\pipe\podman-bashy`); err == nil {
		t.Fatal("Dial(pipe) off Windows: want error, got nil")
	}
}

// TestProbeMissing covers the negative path Probe is actually used for:
// the admin UI greys out a daemon toggle when nothing answers.
func TestProbeMissing(t *testing.T) {
	if Probe("", time.Millisecond) {
		t.Error("Probe(\"\") = true, want false")
	}
	if Probe(filepath.Join(t.TempDir(), "nope.sock"), 50*time.Millisecond) {
		t.Error("Probe(nonexistent) = true, want false")
	}
	// A REGULAR FILE is not a socket. On unix the mode check rejects it;
	// on Windows there is no mode bit, so the dial is what rejects it.
	// Either way the answer must be false — a stale file left behind by a
	// dead daemon must never read as "available".
	reg := filepath.Join(t.TempDir(), "regular")
	if err := os.WriteFile(reg, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if Probe(reg, 50*time.Millisecond) {
		t.Error("Probe(regular file) = true, want false")
	}
}

// TestProbeLiveUnixSocket proves the positive path against a real
// listener, so Probe isn't merely "always false".
func TestProbeLiveUnixSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix-socket listener path; windows is covered by the pipe branch on a real host")
	}
	// macOS caps sun_path at 104 bytes, so keep the path short rather
	// than using the long t.TempDir() under /var/folders.
	dir, err := os.MkdirTemp("/tmp", "ls")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	if !Probe(sock, 2*time.Second) {
		t.Error("Probe(live unix socket) = false, want true")
	}
	if got := Scheme(sock); got != SchemeUnix {
		t.Errorf("Scheme = %q, want %q", got, SchemeUnix)
	}
}
