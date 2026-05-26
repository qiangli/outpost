package upgrade

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

// fakeOutpostBinary returns the path to a small Go-built program that
// pretends to be an outpost binary for probe-only purposes: it
// answers `version --json` with the provided BuildInfo-shaped JSON
// (verbatim) and exits with the requested status. Everything else
// exits non-zero. Lets us drive Probe without coupling to the real
// outpost binary or its release sha.
func fakeOutpostBinary(t *testing.T, jsonBody string, exit int) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "fake.go")
	body := "package main\nimport (\"fmt\"; \"os\")\nfunc main() {\nif len(os.Args) >= 3 && os.Args[1] == \"version\" && os.Args[2] == \"--json\" {\n  fmt.Print(`" + jsonBody + "`)\n  os.Exit(" + strconv.Itoa(exit) + ")\n}\nos.Exit(2)\n}\n"
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "fake")
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	c := exec.Command("go", "build", "-o", out, src)
	c.Dir = dir
	c.Stdout = os.Stderr
	c.Stderr = os.Stderr
	if err := c.Run(); err != nil {
		t.Fatal(err)
	}
	return out
}
