package bashypath

import (
	"os"
	"path/filepath"
	"testing"
)

func executableFixture(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, Name())
	if err := os.WriteFile(p, []byte("fixture"), 0755); err != nil {
		t.Fatal(err)
	}
	return p
}
func TestFindPrefersSiblingThenOverrideThenPATH(t *testing.T) {
	siblingDir, overrideDir, pathDir := t.TempDir(), t.TempDir(), t.TempDir()
	sibling, override, path := executableFixture(t, siblingDir), executableFixture(t, overrideDir), executableFixture(t, pathDir)
	t.Setenv("OUTPOST_BASHY_BIN", override)
	t.Setenv("PATH", pathDir)
	executable := func() (string, error) { return filepath.Join(siblingDir, "outpost"), nil }
	for _, tc := range []struct {
		want    string
		managed bool
	}{{sibling, true}, {override, false}, {path, false}} {
		got, managed, err := Find(executable)
		if err != nil || got != tc.want || managed != tc.managed {
			t.Fatalf("Find = %q,%v,%v; want %q,%v", got, managed, err, tc.want, tc.managed)
		}
		if err = os.Remove(tc.want); err != nil {
			t.Fatal(err)
		}
		if tc.want == override {
			t.Setenv("OUTPOST_BASHY_BIN", "")
		}
	}
}
func TestSiblingFollowsExecutableInstallLink(t *testing.T) {
	install, links := t.TempDir(), t.TempDir()
	executable := filepath.Join(install, "outpost")
	if err := os.WriteFile(executable, []byte("fixture"), 0755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(links, "outpost")
	if err := os.Symlink(executable, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	// TempDir may itself sit behind a symlink (macOS /var -> /private/var).
	realInstall, err := filepath.EvalSymlinks(install)
	if err != nil {
		t.Fatal(err)
	}
	if got := Sibling(func() (string, error) { return link, nil }); got != filepath.Join(realInstall, Name()) {
		t.Fatalf("Sibling = %q; want %q", got, filepath.Join(realInstall, Name()))
	}
}
func TestExecutableRejectsDirectories(t *testing.T) {
	if Executable(t.TempDir()) {
		t.Fatal("directory accepted as an executable")
	}
}
