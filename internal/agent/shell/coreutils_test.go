package shell

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// runWithCoreutils runs one command line through an interp.Runner wired
// exactly like the production sites (CoreutilsExec middleware), with a
// caller-controlled PATH and working directory. Returns stdout+stderr
// and the exit code.
func runWithCoreutils(t *testing.T, dir, path, command string) (string, int) {
	t.Helper()
	var out bytes.Buffer
	runner, err := interp.New(
		interp.StdIO(strings.NewReader(""), &out, &out),
		interp.Env(expand.ListEnviron("PATH="+path)),
		interp.Dir(dir),
		interp.ExecHandlers(CoreutilsExec),
	)
	if err != nil {
		t.Fatalf("interp.New: %v", err)
	}
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		t.Fatalf("parse %q: %v", command, err)
	}
	code := 0
	if err := runner.Run(context.Background(), file); err != nil {
		var ec interp.ExitStatus
		if !stdErrorsAs(err, &ec) {
			t.Fatalf("run %q: %v", command, err)
		}
		code = int(ec)
	}
	return out.String(), code
}

// TestCoreutilsFallbackFiresOffPATH is the Windows scenario: the
// platform offers no `cat`/`head`/`whoami`, so the embedded registry
// must serve them. Simulated by pointing PATH at an empty dir.
func TestCoreutilsFallbackFiresOffPATH(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "data.txt"), []byte("hello-from-coreutils\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	emptyBin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(emptyBin, 0o755); err != nil {
		t.Fatal(err)
	}

	out, code := runWithCoreutils(t, dir, emptyBin, "cat data.txt")
	if code != 0 {
		t.Fatalf("cat exit = %d, output: %s", code, out)
	}
	if !strings.Contains(out, "hello-from-coreutils") {
		t.Errorf("cat output = %q, want file contents", out)
	}

	// Relative operands must resolve against the runner's cwd, not the
	// test process's — head exercises the Dir threading a second way.
	out, code = runWithCoreutils(t, dir, emptyBin, "head -n 1 data.txt")
	if code != 0 || !strings.Contains(out, "hello-from-coreutils") {
		t.Errorf("head exit=%d output=%q", code, out)
	}
}

// TestCoreutilsFallbackPATHWins: a real executable shadowing a registry
// name must win — the platform userland is authoritative, the embedded
// tools only fill gaps. (Unix-only: the shim is a shell script.)
func TestCoreutilsFallbackPATHWins(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script shim needs unix")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	shim := "#!/bin/sh\necho SYSTEM-CAT\n"
	if err := os.WriteFile(filepath.Join(bin, "cat"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}

	out, code := runWithCoreutils(t, dir, bin, "cat anything-at-all")
	if code != 0 {
		t.Fatalf("shim cat exit = %d, output: %s", code, out)
	}
	if !strings.Contains(out, "SYSTEM-CAT") {
		t.Errorf("PATH shim should win over embedded cat, got: %q", out)
	}
}

// TestCoreutilsFallbackUnknownStays127: names in neither PATH nor the
// registry keep the standard not-found behavior.
func TestCoreutilsFallbackUnknownStays127(t *testing.T) {
	dir := t.TempDir()
	emptyBin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(emptyBin, 0o755); err != nil {
		t.Fatal(err)
	}
	_, code := runWithCoreutils(t, dir, emptyBin, "definitely-not-a-tool-xyz")
	if code != 127 {
		t.Errorf("unknown command exit = %d, want 127", code)
	}
}

// TestCoreutilsFallbackNonzeroExit: tool exit codes propagate as shell
// exit status (cat on a missing file is a GNU exit-1).
func TestCoreutilsFallbackNonzeroExit(t *testing.T) {
	dir := t.TempDir()
	emptyBin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(emptyBin, 0o755); err != nil {
		t.Fatal(err)
	}
	out, code := runWithCoreutils(t, dir, emptyBin, "cat no-such-file.txt")
	if code == 0 {
		t.Errorf("cat of missing file should fail, output: %q", out)
	}
}
