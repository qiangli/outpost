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
	t.Setenv("OUTPOST_BASHY_BIN", "")
	t.Setenv("PATH", empty)
	t.Setenv("HOME", empty)
	// Guard against a system-wide bashy in a hardcoded candidate dir.
	for _, sys := range []string{"/usr/local/bin/bashy", "/opt/homebrew/bin/bashy"} {
		if isExecutableFile(sys) {
			t.Skipf("system bashy present at %s; backoff path not exercised here", sys)
		}
	}
	r := &bashyBinaryResolver{lastFetch: time.Now()} // within backoff window
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
