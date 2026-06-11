// Copyright (c) 2026, the outpost authors
// See LICENSE for licensing information

package shell

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// readN reads exactly n bytes from r with a test deadline.
func readN(t *testing.T, r io.Reader, n int) string {
	t.Helper()
	buf := make([]byte, n)
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(r, buf)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("read %d bytes: %v", n, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("read %d bytes: timeout", n)
	}
	return string(buf)
}

// TestVPTY_ONLCR proves runner output (slave writes) reaches the master
// with \n expanded to \r\n — without it a raw remote terminal renders
// stair-stepped lines.
func TestVPTY_ONLCR(t *testing.T) {
	master, slave, err := openVPTY()
	if err != nil {
		t.Fatalf("openVPTY: %v", err)
	}
	defer master.Close()
	defer slave.Close()

	go func() { _, _ = slave.Write([]byte("a\nb\r\nc")) }()
	// Pre-existing \r\n picks up an extra \r — same as kernel ONLCR,
	// and a no-op for the terminal.
	if got, want := readN(t, master, 8), "a\r\nb\r\r\nc"; got != want {
		t.Fatalf("master read %q, want %q", got, want)
	}
}

// TestVPTY_ICRNL proves keystrokes (master writes) reach the slave with
// \r translated to \n, so line-reading commands (`read`, `cat`) see the
// newline a cooked TTY would deliver. readline treats \n (^J) as Enter,
// so the interactive path is unaffected.
func TestVPTY_ICRNL(t *testing.T) {
	master, slave, err := openVPTY()
	if err != nil {
		t.Fatalf("openVPTY: %v", err)
	}
	defer master.Close()
	defer slave.Close()

	go func() { _, _ = master.Write([]byte("hi\rthere")) }()
	if got, want := readN(t, slave, 8), "hi\nthere"; got != want {
		t.Fatalf("slave read %q, want %q", got, want)
	}
}

// TestVPTY_WindowSize covers the geometry plumbing: defaults, resize via
// sessionSetSize (the path Session.Resize takes), and the virtualTTY
// view readline polls.
func TestVPTY_WindowSize(t *testing.T) {
	master, slave, err := openVPTY()
	if err != nil {
		t.Fatalf("openVPTY: %v", err)
	}
	defer master.Close()
	defer slave.Close()

	if c, r := slave.WindowSize(); c != 80 || r != 24 {
		t.Fatalf("default size = %dx%d, want 80x24", c, r)
	}
	if err := sessionSetSize(master, 120, 40); err != nil {
		t.Fatalf("sessionSetSize: %v", err)
	}
	if c, r := slave.WindowSize(); c != 120 || r != 40 {
		t.Fatalf("size after resize = %dx%d, want 120x40", c, r)
	}
}

// TestVPTY_CloseUnblocksPeer locks in the teardown semantics the SSH and
// WebSocket glue rely on: closing one end EOFs the peer's reads and
// errors the peer's writes, and Close is idempotent.
func TestVPTY_CloseUnblocksPeer(t *testing.T) {
	master, slave, err := openVPTY()
	if err != nil {
		t.Fatalf("openVPTY: %v", err)
	}

	readErr := make(chan error, 1)
	go func() {
		_, err := slave.Read(make([]byte, 1))
		readErr <- err
	}()
	if err := master.Close(); err != nil {
		t.Fatalf("master.Close: %v", err)
	}
	select {
	case err := <-readErr:
		if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("slave read after master close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("slave read did not unblock after master close")
	}
	if _, err := slave.Write([]byte("x")); err == nil {
		t.Fatal("slave write after master close: want error")
	}
	if err := master.Close(); err != nil {
		t.Fatalf("second master.Close: %v", err)
	}
	_ = slave.Close()
}

// TestVPTY_InteractiveSession runs the full Windows-shaped session —
// virtual pair + AssumeTTY readline — on whatever platform the tests
// run on. This is the regression test for "sshd on windows: PTY not
// supported on Windows v1": the same wiring NewSession produces on
// Windows, driven the way the SSH server drives it.
func TestVPTY_InteractiveSession(t *testing.T) {
	master, slave, err := openVPTY()
	if err != nil {
		t.Fatalf("openVPTY: %v", err)
	}
	s, err := newSessionFrom(master, slave, SessionOptions{Term: "xterm-256color", Cols: 100, Rows: 30})
	if err != nil {
		t.Fatalf("newSessionFrom: %v", err)
	}
	defer s.Close()

	out := newPtyDrain(s.Master())
	defer out.stop()
	// Registered after out.stop() so it runs first (LIFO) — the drain
	// only unblocks once the session's pipes are closed.
	defer s.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- s.Run(ctx) }()

	// Wait for the first prompt (greeting ends with a prompt render).
	waitFor := func(needle, phase string) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			if strings.Contains(out.snapshot(), needle) {
				return
			}
			select {
			case <-deadline:
				t.Fatalf("%s: %q never appeared; output:\n%s", phase, needle, out.snapshot())
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	waitFor("Matrix shell", "greeting")

	// A raw remote terminal sends \r for Enter.
	if _, err := io.WriteString(s.Master(), "echo windows-vpty-ok\r"); err != nil {
		t.Fatalf("write command: %v", err)
	}
	waitFor("windows-vpty-ok\r\n", "command output (with ONLCR ending)")

	if _, err := io.WriteString(s.Master(), "exit\r"); err != nil {
		t.Fatalf("write exit: %v", err)
	}
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Fatalf("session did not exit; output:\n%s", out.snapshot())
	}
	<-runErrCh
}
