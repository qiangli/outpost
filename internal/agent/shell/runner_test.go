// Copyright (c) 2026, the outpost authors
// See LICENSE for licensing information

package shell

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestSession_ExitBuiltinEndsSession types `exit` and expects the run loop
// to terminate like Ctrl-D would. Used to be a no-op because the run loop
// only watched the error return, but `exit` with code 0 returns nil — the
// session stayed alive until the PTY was closed externally.
func TestSession_ExitBuiltinEndsSession(t *testing.T) {
	testSessionTerminatesAfter(t, "exit\n")
}

// TestSession_ExitWithCodeEndsSession covers `exit 42`. Previously this
// happened to work — but for the wrong reason (`isExit` matched any
// ExitStatus error), so the same code path also killed the session on
// every command failure. Locks in that exit-with-code still ends things.
func TestSession_ExitWithCodeEndsSession(t *testing.T) {
	testSessionTerminatesAfter(t, "exit 42\n")
}

// TestSession_RunOncePTYTty proves that RunOnce attaches the command
// to a real PTY — the regression for "ssh -tt host tty" returning
// "not a tty" when the SSH exec path skipped PTY allocation.
func TestSession_RunOncePTYTty(t *testing.T) {
	s, err := NewSession(SessionOptions{Term: "dumb", Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer s.Close()

	out := newPtyDrain(s.Master())
	defer out.stop()
	defer s.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	statusCh := make(chan uint32, 1)
	go func() { statusCh <- s.RunOnce(ctx, "tty") }()

	select {
	case status := <-statusCh:
		if status != 0 {
			t.Fatalf("tty exited %d, want 0\noutput:\n%s", status, out.snapshot())
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("tty did not finish; output so far:\n%s", out.snapshot())
	}
	snap := out.snapshot()
	if !strings.Contains(snap, "/dev/") {
		t.Fatalf("tty output should name a /dev/tty path, got:\n%s", snap)
	}
}

// TestSession_RunOnceExitCode confirms the runner returns the command's
// exit status correctly (not always 0).
func TestSession_RunOnceExitCode(t *testing.T) {
	s, err := NewSession(SessionOptions{Term: "dumb", Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer s.Close()
	out := newPtyDrain(s.Master())
	defer out.stop()
	defer s.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	statusCh := make(chan uint32, 1)
	go func() { statusCh <- s.RunOnce(ctx, "exit 42") }()
	select {
	case status := <-statusCh:
		if status != 42 {
			t.Fatalf("status=%d, want 42", status)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("RunOnce hung; output:\n%s", out.snapshot())
	}
}

// TestSession_InvalidCommandKeepsSessionAlive is the regression test for
// the "shell blowup" bug: typing a non-existent command used to terminate
// the entire session because the loop treated ExitStatus(127) as an exit
// signal.
func TestSession_InvalidCommandKeepsSessionAlive(t *testing.T) {
	s, err := NewSession(SessionOptions{Term: "dumb", Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer s.Close()

	out := newPtyDrain(s.Master())
	// Order matters: Close releases the PTY which unblocks pump → stop.
	defer out.stop()
	defer s.Close()

	runErrCh := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { runErrCh <- s.Run(ctx) }()

	// Wait for the greeting + first prompt.
	if !out.waitFor("$ ", 2*time.Second) && !out.waitFor("# ", 2*time.Second) {
		t.Fatalf("never saw first prompt; output so far:\n%s", out.snapshot())
	}

	if _, err := io.WriteString(s.Master(), "no_such_command_zzzzz\n"); err != nil {
		t.Fatalf("write bad command: %v", err)
	}

	// Session must still be alive: a second prompt must appear after the
	// error message. We strip what we've seen so far and wait for a fresh
	// "$ " / "# ".
	out.discardSnapshot()
	if !out.waitFor("$ ", 2*time.Second) && !out.waitFor("# ", 2*time.Second) {
		t.Fatalf("session ended after bad command; output so far:\n%s", out.snapshot())
	}

	// Now end it cleanly so the test exits.
	if _, err := io.WriteString(s.Master(), "exit\n"); err != nil {
		t.Fatalf("write exit: %v", err)
	}
	select {
	case <-s.Done():
	case <-time.After(2 * time.Second):
		t.Fatalf("exit did not terminate session after bad command")
	}
	<-runErrCh
}

func testSessionTerminatesAfter(t *testing.T, line string) {
	t.Helper()
	s, err := NewSession(SessionOptions{Term: "dumb", Cols: 80, Rows: 24})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	defer s.Close()

	out := newPtyDrain(s.Master())
	// Order matters: Close releases the PTY which unblocks pump → stop.
	defer out.stop()
	defer s.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runErrCh := make(chan error, 1)
	go func() { runErrCh <- s.Run(ctx) }()

	if !out.waitFor("$ ", 2*time.Second) && !out.waitFor("# ", 2*time.Second) {
		t.Fatalf("never saw first prompt; output so far:\n%s", out.snapshot())
	}

	if _, err := io.WriteString(s.Master(), line); err != nil {
		t.Fatalf("write %q: %v", line, err)
	}

	select {
	case <-s.Done():
	case <-time.After(2 * time.Second):
		t.Fatalf("session did not terminate after %q; output so far:\n%s", line, out.snapshot())
	}
	<-runErrCh
}

// ptyDrain pumps a PTY master fd into an in-memory buffer that tests can
// poll. We keep it small and intentionally synchronous around the buffer
// — these tests run for seconds, not microseconds.
//
// It also acts as a minimal terminal emulator: ergochat/readline (the line
// editor inside mvdan.cc/sh/v3/interactive that Session.Run uses) emits a
// DSR cursor-position query ("\x1b[6n") before printing each prompt and
// blocks until the terminal replies with the matching CPR. In production
// the responder is xterm.js / the SSH client's terminal emulator. In
// tests there is no such emulator behind the PTY, so this pump answers
// the query itself with a stub "\x1b[1;1R" — otherwise Run hangs forever
// waiting for the prompt to render and the test deadlines out.
type ptyDrain struct {
	mu     sync.Mutex
	buf    strings.Builder
	done   chan struct{}
	master *os.File
}

func newPtyDrain(master *os.File) *ptyDrain {
	d := &ptyDrain{done: make(chan struct{}), master: master}
	go d.pump()
	return d
}

func (d *ptyDrain) pump() {
	defer close(d.done)
	buf := make([]byte, 4096)
	const cprQuery = "\x1b[6n"
	var tail []byte // straddler bytes from the previous read (≤ len(cprQuery)-1)
	for {
		n, err := d.master.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			d.mu.Lock()
			d.buf.Write(chunk)
			d.mu.Unlock()

			// Search for the DSR query across the read boundary. tail
			// holds the trailing bytes that could have been the start
			// of a sequence split across reads.
			scan := append(tail, chunk...)
			for {
				idx := bytes.Index(scan, []byte(cprQuery))
				if idx < 0 {
					break
				}
				_, _ = d.master.Write([]byte("\x1b[1;1R"))
				scan = scan[idx+len(cprQuery):]
			}
			if keep := len(cprQuery) - 1; len(scan) > keep {
				tail = append(tail[:0], scan[len(scan)-keep:]...)
			} else {
				tail = append(tail[:0], scan...)
			}
		}
		if err != nil {
			return
		}
	}
}

func (d *ptyDrain) snapshot() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.buf.String()
}

func (d *ptyDrain) discardSnapshot() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.buf.Reset()
}

func (d *ptyDrain) waitFor(needle string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(d.snapshot(), needle) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func (d *ptyDrain) stop() { <-d.done }
