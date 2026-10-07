package shell

import (
	"bytes"
	"context"
	"io"
	"net"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// This tests the process boundary without depending on shell syntax or a Bashy
// build. Existing local/runner tests exercise the real paired executable.
func TestChildLifecycle(t *testing.T) {
	name := "bashy-fixture"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binary := filepath.Join(t.TempDir(), name)
	cmd := exec.Command("go", "build", "-o", binary, "./testdata/child")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build child fixture: %v\n%s", err, out)
	}
	t.Setenv("OUTPOST_BASHY_BIN", binary)
	t.Run("environment-and-exit-status", func(t *testing.T) {
		var out bytes.Buffer
		code, err := RunCommand(context.Background(), "environment", nil, &out, &out, map[string]string{"OUTPOST_TEST_VALUE": "child-value"})
		if err != nil || code != 42 || out.String() != "child-value" {
			t.Fatalf("code=%d err=%v output=%q", code, err, out.String())
		}
	})
	for _, action := range []string{"cancel", "close"} {
		t.Run(action, func(t *testing.T) {
			session, err := NewSession(SessionOptions{Cols: 80, Rows: 24})
			if err != nil {
				t.Fatal(err)
			}
			drain := newPtyDrain(session.Master())
			defer drain.stop()
			defer session.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			finished := make(chan uint32, 1)
			go func() { finished <- session.RunOnce(ctx, "wait") }()
			if !drain.waitFor("child-ready", 3*time.Second) {
				t.Fatalf("child did not start: %s", drain.snapshot())
			}
			if err = session.Resize(101, 37); err != nil {
				t.Fatalf("resize: %v", err)
			}
			// Consoles complete input lines on CR; a bare LF never reaches the
			// child on Windows while Unix PTYs accept either terminator.
			if _, err = io.WriteString(session.Master(), "size\r\n"); err != nil {
				t.Fatal(err)
			}
			if !drain.waitFor("size=101x37", 3*time.Second) {
				t.Fatalf("child did not observe resize: %s", drain.snapshot())
			}
			if action == "cancel" {
				cancel()
			} else {
				_ = session.Close()
			}
			select {
			case <-finished:
			case <-time.After(3 * time.Second):
				t.Fatal("child survived session teardown")
			}
		})
	}
	t.Run("cancel-descendants", func(t *testing.T) {
		session, err := NewSession(SessionOptions{Cols: 80, Rows: 24})
		if err != nil {
			t.Fatal(err)
		}
		drain := newPtyDrain(session.Master())
		defer drain.stop()
		defer session.Close()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		finished := make(chan uint32, 1)
		go func() { finished <- session.RunOnce(ctx, "tree") }()
		address := waitForChildListener(t, drain.snapshot)
		cancel()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Fatal("shell child survived cancellation")
		}
		assertChildListenerClosed(t, address)
	})

	t.Run("cancel-non-pty-descendants", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		output := &lockedOutput{}
		finished := make(chan struct{})
		go func() { defer close(finished); _, _ = RunCommand(ctx, "tree", nil, output, output, nil) }()
		address := waitForChildListener(t, output.String)
		cancel()
		select {
		case <-finished:
		case <-time.After(6 * time.Second):
			t.Fatal("non-PTY command survived cancellation")
		}
		assertChildListenerClosed(t, address)
	})

}

// This catches cancellation that kills only Bashy and leaves a running child
// behind. A bound listener proves liveness without confusing zombies with live
// processes on hosts whose init reaps asynchronously.
func waitForChildListener(t *testing.T, output func() string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, rest, found := strings.Cut(output(), "child-listener=")
		line, _, complete := strings.Cut(rest, "\n")
		address := strings.TrimSpace(line)
		_, port, parseErr := net.SplitHostPort(address)
		if found && complete && parseErr == nil && port != "" {
			conn, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
			if err == nil {
				_ = conn.Close()
				return address
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("descendant listener never became reachable: %s", output())
	return ""
}

func assertChildListenerClosed(t *testing.T, address string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err != nil {
			return
		}
		_ = conn.Close()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("descendant listener %s survived cancellation", address)
}

type lockedOutput struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *lockedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
func (b *lockedOutput) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.buf.String() }
