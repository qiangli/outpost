package shell

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/expand"
)

// pathFromEnviron pulls PATH out of an expand.Environ.
func pathFromEnviron(t *testing.T, env expand.Environ) string {
	t.Helper()
	v := env.Get("PATH")
	return v.String()
}

func TestBuildEnv_PrependsExeDir(t *testing.T) {
	// Use a known-bare PATH so we can assert the helper's additions
	// without depending on the developer's $PATH.
	t.Setenv("PATH", "/usr/bin:/bin")

	env := BuildEnv()
	got := pathFromEnviron(t, env)

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	exeDir := filepath.Dir(exe)
	if !strings.Contains(got, exeDir) {
		t.Errorf("PATH=%q should contain outpost exe dir %q", got, exeDir)
	}

	// PATH ordering: exe dir should appear BEFORE the original entries
	// so a command named like an existing system binary still resolves
	// to outpost's bundled version first.
	idxExe := strings.Index(got, exeDir)
	idxUsrBin := strings.Index(got, "/usr/bin")
	if idxExe < 0 || idxUsrBin < 0 || idxExe > idxUsrBin {
		t.Errorf("expected exe dir before /usr/bin in PATH=%q", got)
	}
}

func TestBuildEnv_PreservesExistingPathEntries(t *testing.T) {
	t.Setenv("PATH", "/foo/bin:/bar/bin:/usr/bin")
	env := BuildEnv()
	got := pathFromEnviron(t, env)
	for _, want := range []string{"/foo/bin", "/bar/bin", "/usr/bin"} {
		if !strings.Contains(got, want) {
			t.Errorf("PATH lost original entry %q: %q", want, got)
		}
	}
}

func TestBuildEnv_NoDuplicatesOnRepeatExtras(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	exeDir := filepath.Dir(exe)
	// Pre-set PATH so exeDir is ALREADY there; helper should not add
	// it a second time.
	t.Setenv("PATH", exeDir+":/usr/bin")

	env := BuildEnv()
	got := pathFromEnviron(t, env)
	if strings.Count(got, exeDir) > 1 {
		t.Errorf("exeDir appears %d times in PATH=%q (want 1)",
			strings.Count(got, exeDir), got)
	}
}

func TestBuildEnv_SkipsNonexistentDirs(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	env := BuildEnv()
	got := pathFromEnviron(t, env)
	// Both of these are dirs that almost certainly don't exist on a
	// random dev machine; if Stat says they don't exist, BuildEnv
	// must not add them.
	for _, p := range []string{"/nonexistent-prefix-from-test-zzz/bin"} {
		if strings.Contains(got, p) {
			t.Errorf("PATH should not contain nonexistent dir %q: %q", p, got)
		}
	}
}

func TestBuildEnvWith_AppendsNewKey(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	os.Unsetenv("TERM")

	env := BuildEnvWith(map[string]string{"TERM": "xterm-256color"})
	if got := env.Get("TERM").String(); got != "xterm-256color" {
		t.Errorf("TERM=%q, want %q", got, "xterm-256color")
	}
}

func TestBuildEnvWith_ReplacesExistingKey(t *testing.T) {
	// Outpost daemon's own TERM (often "dumb" or empty in launchd). The
	// override from a real SSH client's pty-req must win.
	t.Setenv("TERM", "dumb")
	t.Setenv("PATH", "/usr/bin")

	env := BuildEnvWith(map[string]string{"TERM": "xterm-256color"})
	if got := env.Get("TERM").String(); got != "xterm-256color" {
		t.Errorf("TERM=%q, want %q (override should win over inherited)", got, "xterm-256color")
	}
}

func TestBuildEnvWith_NilEqualsBuildEnv(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("TERM", "dumb")

	base := BuildEnv().Get("TERM").String()
	got := BuildEnvWith(nil).Get("TERM").String()
	if base != got {
		t.Errorf("BuildEnvWith(nil) diverged from BuildEnv: %q vs %q", got, base)
	}
}

func TestBuildEnvWith_PreservesPathExtras(t *testing.T) {
	// The PATH-extras logic must still run when overrides are provided.
	t.Setenv("PATH", "/usr/bin:/bin")

	env := BuildEnvWith(map[string]string{"TERM": "xterm-256color"})
	got := pathFromEnviron(t, env)
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	exeDir := filepath.Dir(exe)
	if !strings.Contains(got, exeDir) {
		t.Errorf("BuildEnvWith dropped PATH-extras: %q missing %q", got, exeDir)
	}
}

func TestAugmentPathEntries_WindowsAddsCommonDirs(t *testing.T) {
	extras := windowsPathExtras([]string{`SystemRoot=C:\Windows`}, "windows")
	exists := func(string) bool { return true }

	got := augmentPathEntries([]string{`C:\outpost`}, extras, "windows", exists)
	want := []string{
		`C:\Windows\System32`,
		`C:\Windows`,
		`C:\Windows\System32\Wbem`,
		`C:\Windows\System32\WindowsPowerShell\v1.0`,
		`C:\Windows\System32\OpenSSH`,
		`C:\Program Files\NVIDIA Corporation\NVSMI`,
		`C:\outpost`,
	}
	if strings.Join(got, ";") != strings.Join(want, ";") {
		t.Fatalf("PATH entries = %q, want %q", strings.Join(got, ";"), strings.Join(want, ";"))
	}
}

func TestAugmentPathEntries_WindowsSkipsMissingCommonDirs(t *testing.T) {
	extras := windowsPathExtras([]string{`SystemRoot=C:\Windows`}, "windows")
	exists := func(p string) bool {
		return p == `C:\Windows\System32` || p == `C:\outpost`
	}

	got := augmentPathEntries([]string{`C:\outpost`}, extras, "windows", exists)
	want := []string{`C:\Windows\System32`, `C:\outpost`}
	if strings.Join(got, ";") != strings.Join(want, ";") {
		t.Fatalf("PATH entries = %q, want %q", strings.Join(got, ";"), strings.Join(want, ";"))
	}
}

func TestAugmentPathEntries_WindowsDedupesCaseInsensitive(t *testing.T) {
	extras := windowsPathExtras([]string{`SystemRoot=C:\Windows`}, "windows")
	exists := func(string) bool { return true }

	got := augmentPathEntries([]string{
		`c:\windows\system32`,
		`C:\outpost`,
		`c:\program files\nvidia corporation\nvsmi`,
	}, extras, "windows", exists)

	for _, wantOnce := range []string{
		`C:\Windows\System32`,
		`C:\Program Files\NVIDIA Corporation\NVSMI`,
	} {
		count := 0
		for _, p := range got {
			if strings.EqualFold(p, wantOnce) {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("%q appears %d times in PATH entries %q, want 1", wantOnce, count, strings.Join(got, ";"))
		}
	}
}

func TestWindowsPathExtras_UsesWindowsEnvCaseInsensitive(t *testing.T) {
	got := windowsPathExtras([]string{`windir=D:\WinDir\`}, "windows")
	if len(got) == 0 {
		t.Fatal("windowsPathExtras returned no entries")
	}
	if got[0] != `D:\WinDir\System32` {
		t.Fatalf("first Windows PATH extra = %q, want %q", got[0], `D:\WinDir\System32`)
	}
}
