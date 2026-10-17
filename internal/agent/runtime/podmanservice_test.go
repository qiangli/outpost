package runtime

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestManagedPodmanService(t *testing.T) {
	// Re-exec only this test as a fake podman, never a real container runtime.
	if os.Getenv("OUTPOST_FAKE_PODMAN") == "1" {
		args := append([]string{os.Args[0]}, os.Args[3:]...)
		if len(args) != 5 || args[1] != "system" || args[2] != "service" || args[3] != "--time=0" {
			os.Exit(2)
		}
		if os.Getenv("CONTAINERS_CONF_OVERRIDE") == "" {
			os.Exit(3)
		}
		ln, err := net.Listen("unix", args[4][7:])
		if err != nil {
			os.Exit(4)
		}
		http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/_ping" {
				w.WriteHeader(404)
				return
			}
			w.Write([]byte("OK"))
		}))
		os.Exit(0)
	}
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket service is Linux-only; fake launcher requires sh")
	}
	t.Setenv("OUTPOST_FAKE_PODMAN", "1")
	t.Setenv("CONTAINERS_CONF_OVERRIDE", "/fake/containers.conf")
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// The test executable needs -test.run, so use a tiny launcher on Unix.
	dir, err := os.MkdirTemp("", "op-podman-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	launcher := filepath.Join(dir, "podman")
	script := "#!/bin/sh\nexec '" + bin + "' -test.run=TestManagedPodmanService -- \"$@\"\n"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := os.WriteFile(launcher, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "api.sock")
	got, err := startPodmanService(ctx, launcher, socket, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got != socket {
		t.Fatal(got)
	}
	// Existing healthy sockets are reused without invoking a second binary.
	if _, err := startPodmanService(ctx, "/nonexistent", socket, time.Second); err != nil {
		t.Fatal(err)
	}
	cancel()
}

func TestManagedPodmanServiceFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix service")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := startPodmanService(ctx, "/nonexistent/podman", filepath.Join(t.TempDir(), "api.sock"), time.Second); err == nil {
		t.Fatal("missing binary succeeded")
	}
	bin := filepath.Join(t.TempDir(), "podman")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexit 7\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := startPodmanService(ctx, bin, filepath.Join(t.TempDir(), "api.sock"), time.Second); err == nil {
		t.Fatal("failed process succeeded")
	}
}
