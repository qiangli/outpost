package main

import (
	"net"
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestBootPathPrefixesPerOS(t *testing.T) {
	linux := bootPathPrefixes("linux")
	for _, want := range []string{"/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin"} {
		if !containsStr(linux, want) {
			t.Errorf("linux prefixes missing %q: %v", want, linux)
		}
	}
	if containsStr(linux, "/opt/homebrew/bin") {
		t.Errorf("linux prefixes must not carry the Homebrew dir: %v", linux)
	}
	darwin := bootPathPrefixes("darwin")
	if !containsStr(darwin, "/opt/homebrew/bin") {
		t.Errorf("darwin prefixes missing Homebrew dir: %v", darwin)
	}
	t.Setenv("SystemRoot", `C:\Windows`)
	win := bootPathPrefixes("windows")
	for _, want := range []string{`C:\Windows\System32`, `C:\Windows`, `C:\Windows\System32\WindowsPowerShell\v1.0`} {
		if !containsStrFold(win, want) {
			t.Errorf("windows prefixes missing %q: %v", want, win)
		}
	}
}

func containsStr(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

func containsStrFold(hay []string, needle string) bool {
	for _, h := range hay {
		if strings.EqualFold(h, needle) {
			return true
		}
	}
	return false
}

func TestMissingPathEntries(t *testing.T) {
	sep := string(os.PathListSeparator)
	got := missingPathEntries(strings.Join([]string{"/usr/bin", "/bin"}, sep),
		[]string{"/usr/bin", "/sbin"}, false)
	if len(got) != 1 || got[0] != "/sbin" {
		t.Fatalf("missing = %v, want [/sbin]", got)
	}
	// Windows compares case-insensitively: a differently-cased
	// System32 still counts as present. Windows lists split on `;`
	// (drive letters contain `:`), even in tests running on unix.
	got = missingPathEntries(`c:\windows\system32;C:\Windows`,
		[]string{`C:\Windows\System32`, `C:\Windows`}, true)
	if len(got) != 0 {
		t.Fatalf("case-insensitive missing = %v, want none", got)
	}
}

func TestEnsureBootPATHHealsAndIdempotent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix PATH layout; windows covered by missingPathEntries + prefixes")
	}
	sep := string(os.PathListSeparator)
	t.Setenv("PATH", strings.Join([]string{"/usr/bin", "/bin"}, sep))
	ensureBootPATH()
	after := os.Getenv("PATH")
	for _, want := range bootPathPrefixes(runtime.GOOS) {
		found := false
		for _, e := range strings.Split(after, sep) {
			if e == want {
				found = true
			}
		}
		if !found {
			t.Errorf("healed PATH missing %q: %s", want, after)
		}
	}
	// Pre-existing entries survive, and a second call adds nothing.
	if !strings.Contains(after, "/usr/bin") {
		t.Errorf("healed PATH dropped an existing entry: %s", after)
	}
	once := after
	ensureBootPATH()
	if twice := os.Getenv("PATH"); twice != once {
		t.Errorf("second ensure changed PATH:\n%s\nvs\n%s", once, twice)
	}
	if ok, detail := bootPathSane(); !ok {
		t.Fatalf("PATH should be sane after healing: %s", detail)
	}
}

func TestBootPathSaneReportsMissing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix PATH layout")
	}
	t.Setenv("PATH", "/nowhere")
	ok, detail := bootPathSane()
	if ok {
		t.Fatal("bare PATH reported sane")
	}
	if !strings.Contains(detail, "/usr/bin") {
		t.Fatalf("detail should name missing entries, got %q", detail)
	}
}

func TestSSHDListeningProbe(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	t.Setenv("OUTPOST_SSHD_PROBE_ADDR", ln.Addr().String())
	if !sshdListening() {
		t.Fatal("sshdListening = false with a listener up; want true")
	}
	addr := ln.Addr().String()
	ln.Close()
	// A closed listener refuses immediately — no sleep needed.
	t.Setenv("OUTPOST_SSHD_PROBE_ADDR", addr)
	if sshdListening() {
		t.Fatal("sshdListening = true after listener closed; want false")
	}
}

func TestSSHDProbeDefaultIsLoopback(t *testing.T) {
	if !strings.HasPrefix(defaultSSHDProbeAddr, "127.0.0.1:") {
		t.Fatalf("default probe %q must be a loopback literal (no hostnames)", defaultSSHDProbeAddr)
	}
	if _, err := net.ResolveTCPAddr("tcp", defaultSSHDProbeAddr); err != nil {
		t.Fatalf("default probe addr: %v", err)
	}
}
