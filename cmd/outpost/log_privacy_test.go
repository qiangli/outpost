package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/secrets"
)

func TestProtectDaemonLogs(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", root)
	dir := filepath.Join(root, "outpost")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	names := []string{"daemon.log", "bashy-supervisord.log", "supervisord.log", "outpost.log", "cli-trace.log"}
	for _, name := range names {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("existing log\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := protectDaemonLogs(); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		p := filepath.Join(dir, name)
		content, err := os.ReadFile(p)
		if err != nil || string(content) != "existing log\n" {
			t.Fatalf("%s content changed", name)
		}
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
			t.Errorf("%s permissions = %o", name, info.Mode().Perm())
		}
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
		t.Errorf("directory permissions = %o", info.Mode().Perm())
	}
	// Repeated starts preserve both content and privacy.
	if err := protectDaemonLogs(); err != nil {
		t.Fatal(err)
	}
}

func TestProtectDaemonLogsCreatesPrivateDirectory(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", root)
	if err := protectDaemonLogs(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(root, "outpost"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
		t.Errorf("directory permissions = %o", info.Mode().Perm())
	}
}

func TestStartupTraceMasksRegisteredCredential(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", root)
	t.Setenv("OUTPOST_TRACE", "1")
	const token = "synthetic-outpost-credential-1234567890"
	if err := secrets.RegisterHostSecret("OUTPOST_TRACE_TEST", token); err != nil {
		t.Fatal(err)
	}
	originalArgs := os.Args
	os.Args = []string{"outpost", "test-command", token}
	t.Cleanup(func() { os.Args = originalArgs })
	emitStartupTrace()
	data, err := os.ReadFile(filepath.Join(root, "outpost", "cli-trace.log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), token) {
		t.Fatal("registered credential leaked into trace")
	}
	if !strings.Contains(string(data), "test-command") {
		t.Fatal("trace lost ordinary command output")
	}
}
