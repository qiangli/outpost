package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestMatchBashyReleaseAsset(t *testing.T) {
	tests := []struct {
		name   string
		goos   string
		goarch string
		want   bool
	}{
		{"bashy-windows-amd64.zip", "windows", "amd64", true},
		{"bashy-darwin-arm64.tar.gz", "darwin", "arm64", true},
		{"bash-windows-amd64.zip", "windows", "amd64", false},
		{"bashy-linux-arm64.tar.gz", "windows", "amd64", false},
		{"checksums.txt", "windows", "amd64", false},
		{"bashy-scratch-windows-amd64", "windows", "amd64", false},
		{"bashy-scratch-linux-amd64", "linux", "amd64", false},
		{"bashy-scratch-windows-amd64.zip", "windows", "amd64", false},
		{"bashy-windows-amd64.raw", "windows", "amd64", false},
		{"bashy-darwin-arm64.oci", "darwin", "arm64", false},
	}
	for _, tt := range tests {
		if got := matchBashyReleaseAsset(tt.name, tt.goos, tt.goarch); got != tt.want {
			t.Fatalf("matchBashyReleaseAsset(%q, %q, %q) = %v, want %v", tt.name, tt.goos, tt.goarch, got, tt.want)
		}
	}
}

func TestBashyArchiveMember(t *testing.T) {
	got := bashyArchiveMember()
	if runtime.GOOS == "windows" {
		if got != "bashy.exe" {
			t.Fatalf("bashyArchiveMember on windows = %q", got)
		}
		return
	}
	if got != "bashy" {
		t.Fatalf("bashyArchiveMember = %q", got)
	}
}

func TestBashyInstallTarget(t *testing.T) {
	dir := t.TempDir()
	got, err := bashyInstallTarget(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("target should be absolute: %q", got)
	}
	if filepath.Base(got) != bashyArchiveMember() {
		t.Fatalf("target basename = %q, want %q", filepath.Base(got), bashyArchiveMember())
	}
}

func TestInstallBashyExecutable(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "nested", bashyArchiveMember())
	if err := os.WriteFile(src, []byte("binary"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := installBashyExecutable(src, dst); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "binary" {
		t.Fatalf("installed body = %q", body)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(dst)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Fatalf("installed file is not executable: %v", info.Mode())
		}
	}
}

func TestIsExecutableFile(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "run")
	if err := os.WriteFile(exe, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(dir, "data")
	if err := os.WriteFile(plain, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !isExecutableFile(exe) {
		t.Errorf("executable file not detected: %s", exe)
	}
	if isExecutableFile(dir) {
		t.Errorf("directory reported as executable: %s", dir)
	}
	if isExecutableFile(filepath.Join(dir, "missing")) {
		t.Errorf("missing file reported as executable")
	}
	// On unix a non-exec-bit file is not runnable; on windows any file is.
	if got, want := isExecutableFile(plain), runtime.GOOS == "windows"; got != want {
		t.Errorf("isExecutableFile(non-exec) = %v, want %v", got, want)
	}
}

func TestBashyCandidatePaths(t *testing.T) {
	paths := bashyCandidatePaths()
	if len(paths) == 0 {
		t.Fatal("expected candidate paths")
	}
	member := bashyArchiveMember()
	for _, p := range paths {
		if filepath.Base(p) != member {
			t.Errorf("candidate %q does not end in %q", p, member)
		}
	}
}

func TestBashyResolverOverride(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "bashy-override")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OUTPOST_BASHY_BIN", exe)
	r := &bashyBinaryResolver{}
	got, err := r.Path(context.Background())
	if err != nil {
		t.Fatalf("resolve via override: %v", err)
	}
	if got != exe {
		t.Fatalf("resolved %q, want override %q", got, exe)
	}
	// Second call must return the cached path even if the override env is gone.
	t.Setenv("OUTPOST_BASHY_BIN", "")
	got2, err := r.Path(context.Background())
	if err != nil || got2 != exe {
		t.Fatalf("cached resolve = %q,%v; want %q,nil", got2, err, exe)
	}
}

func TestBashyResolverBackoff(t *testing.T) {
	// Force local resolution to miss: no override, empty PATH, isolated HOME.
	empty := t.TempDir()
	cache := t.TempDir()
	t.Setenv("OUTPOST_BASHY_BIN", "")
	t.Setenv("PATH", empty)
	t.Setenv("HOME", empty)
	// os.UserHomeDir/os.UserCacheDir/os.UserConfigDir ignore HOME and XDG on
	// windows (%USERPROFILE% / %LOCALAPPDATA% / %APPDATA%) — isolate those too
	// or a previously self-healed bashy is found and no backoff error occurs.
	t.Setenv("USERPROFILE", empty)
	t.Setenv("LOCALAPPDATA", cache)
	t.Setenv("APPDATA", cache)
	t.Setenv("BASHY_BIN_CACHE", t.TempDir())
	// Pin the sibling seam inside the isolated dir: the default seam is the
	// test binary's own directory, which this test does not control.
	r := &bashyBinaryResolver{
		lastFetch:  time.Now(), // within backoff window
		executable: func() (string, error) { return filepath.Join(empty, "outpost"), nil },
	}
	// Guard against a bashy in any location the resolver actually searches.
	for _, p := range append(bashyCandidatePaths(), filepath.Join(empty, bashyArchiveMember())) {
		if isExecutableFile(p) {
			t.Skipf("bashy present at %s; backoff path not exercised here", p)
		}
	}
	_, err := r.Path(context.Background())
	if err == nil {
		t.Fatal("expected an error when bashy is absent and auto-install is backing off")
	}
	if !strings.Contains(err.Error(), "backing off") {
		t.Fatalf("expected backoff error, got: %v", err)
	}
}

func TestBashyCmdRejectsInstallConflict(t *testing.T) {
	cmd := bashyCmd()
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs([]string{"--install", filepath.Join(t.TempDir(), "bashy"), "--install-dir", t.TempDir()})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected conflict error")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBashySiblingWins(t *testing.T) {
	dir, stale := t.TempDir(), t.TempDir()
	sibling := filepath.Join(dir, bashyArchiveMember())
	for _, p := range []string{sibling, filepath.Join(stale, bashyArchiveMember())} {
		if err := os.WriteFile(p, []byte("never execute fixture"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", stale)
	t.Setenv("OUTPOST_BASHY_BIN", filepath.Join(stale, bashyArchiveMember()))
	r := &bashyBinaryResolver{reconciled: true, executable: func() (string, error) { return filepath.Join(dir, "outpost"), nil }}
	got, err := r.Path(context.Background())
	if err != nil || got != sibling {
		t.Fatalf("sibling = %q, %v", got, err)
	}
}

func TestPairedBashyVersionRetainsChannel(t *testing.T) {
	for _, tc := range []struct{ stamp, want string }{{"v1.2.3-dev", "v1.2.3-dev"}, {"1.2.3", "v1.2.3"}, {"", DefaultBashyVersion}, {"dev", DefaultBashyVersion}} {
		if got := pairedBashyVersion(tc.stamp); got != tc.want {
			t.Errorf("%q -> %q, want %q", tc.stamp, got, tc.want)
		}
	}
	if !sameBashyVersion("1.2.3", "v1.2.3-dev") {
		t.Fatal("product versions should match")
	}
}

func TestReplaceBashyAsideRestoresOnFailure(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "bashy.exe")
	if err := os.WriteFile(dst, []byte("old"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := replaceBashyAside(filepath.Join(dir, "missing"), dst); err == nil {
		t.Fatal("expected missing source error")
	}
	body, err := os.ReadFile(dst)
	if err != nil || string(body) != "old" {
		t.Fatalf("original not restored: %q %v", body, err)
	}
}

func TestBashyCmdForwardsExplicitShellArguments(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses /bin/sh; Windows is covered by install matrix")
	}
	path := filepath.Join(t.TempDir(), "bashy")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	old := bashyResolver
	bashyResolver = &bashyBinaryResolver{cached: path, reconciled: true}
	t.Cleanup(func() { bashyResolver = old })
	cmd := bashyCmd()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{"--", "-c", "echo paired"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if output.String() != "-c\necho paired\n" {
		t.Fatalf("forwarded %q", output.String())
	}
}

func TestBashyResolverInvalidOverrideDoesNotFetch(t *testing.T) {
	t.Setenv("OUTPOST_BASHY_BIN", filepath.Join(t.TempDir(), "missing"))
	r := &bashyBinaryResolver{executable: func() (string, error) { return filepath.Join(t.TempDir(), "outpost"), nil }}
	if _, err := r.Path(context.Background()); err == nil || !strings.Contains(err.Error(), "OUTPOST_BASHY_BIN") {
		t.Fatalf("override error: %v", err)
	}
	if !r.lastFetch.IsZero() {
		t.Fatal("invalid override attempted a download")
	}
}
