package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
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
