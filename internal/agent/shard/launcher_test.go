package shard

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuf is a concurrency-safe log sink so the test can poll it while exec's
// copy goroutine writes.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitFor polls cond until true or the deadline; fails the test on timeout.
func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", d)
}

// writeStub writes a unix shell stand-in for the prima binary: it echoes its
// argv (so the test can assert the wiring produced the right flags) then blocks
// until killed.
func writeStub(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "prima-stub.sh")
	script := "#!/bin/sh\necho \"PRIMA-ARGS: $*\"\nwhile true; do sleep 1; done\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStart_WiresLaunchesAndStops(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stub is unix-only")
	}
	f := newFake()
	plan, _ := ring2().PlanFor(0) // leader: 2 exposes, 2 forwards
	var buf syncBuf
	sess, err := Start(context.Background(), f, plan, LaunchConfig{
		BinaryPath: writeStub(t),
		ModelPath:  "/models/qwen.gguf",
		Extra:      []string{"--prefetch", "-p", "hi", "-n", "8"},
		LogWriter:  &buf,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Mesh wiring is up while prima runs.
	if len(f.exposed) != 2 || len(f.opened) != 2 {
		t.Fatalf("wiring not applied: exposed=%d opened=%d", len(f.exposed), len(f.opened))
	}
	if !sess.Running() {
		t.Error("expected session running")
	}

	// Wait until the stub has echoed its argv (robust under load), then stop.
	waitFor(t, 5*time.Second, func() bool { return strings.Contains(buf.String(), "PRIMA-ARGS:") })
	sess.Stop()

	got := buf.String() // safe: Stop waited for the process (and its log copy) to finish
	for _, want := range []string{
		"--world 2", "--rank 0", "--master 127.0.0.1", "--next 127.0.0.1",
		"--data-port 9000", "-m /models/qwen.gguf", "--prefetch", "-p hi", "-n 8",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prima argv missing %q; got: %s", want, got)
		}
	}
	// Stop tore down the mesh wiring.
	if len(f.exposed) != 0 {
		t.Errorf("Stop did not unexpose: %v", f.exposed)
	}
	for _, ln := range f.opened {
		if !ln.closed {
			t.Error("Stop did not close a forward listener")
		}
	}
	if sess.Running() {
		t.Error("session should be stopped")
	}
	sess.Stop() // idempotent
}

func TestStart_FailClosed_BadBinary(t *testing.T) {
	f := newFake()
	plan, _ := ring2().PlanFor(0)
	_, err := Start(context.Background(), f, plan, LaunchConfig{
		BinaryPath: filepath.Join(t.TempDir(), "does-not-exist"),
		ModelPath:  "/m.gguf",
	})
	if err == nil {
		t.Fatal("expected start to fail on a missing binary")
	}
	// A failed start must unwind the mesh wiring (never a half-formed shard).
	if len(f.exposed) != 0 {
		t.Errorf("fail-closed left services exposed: %v", f.exposed)
	}
	for _, ln := range f.opened {
		if !ln.closed {
			t.Error("fail-closed left a forward listener open")
		}
	}
}

func TestStart_Validation(t *testing.T) {
	f := newFake()
	plan, _ := ring2().PlanFor(0)
	if _, err := Start(context.Background(), f, plan, LaunchConfig{ModelPath: "/m.gguf"}); err == nil {
		t.Error("expected error for empty binary path")
	}
	if _, err := Start(context.Background(), f, plan, LaunchConfig{BinaryPath: "/bin/true"}); err == nil {
		t.Error("expected error for empty model path")
	}
	// Neither validation failure should have wired anything.
	if len(f.exposed) != 0 || len(f.opened) != 0 {
		t.Errorf("validation failure wired the mesh: exposed=%v opened=%d", f.exposed, len(f.opened))
	}
}
