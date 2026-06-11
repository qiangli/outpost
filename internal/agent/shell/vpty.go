package shell

// Virtual PTY: an in-memory emulation of the master/slave pair for
// platforms with no kernel PTY an in-process runner can sit behind.
// Windows is the consumer — ConPTY (CreatePseudoConsole) attaches a
// *child process* to the console, but the matrix-shell / sshd runner is
// the qiangli/sh interpreter living in this very process, so there is
// no child to attach. The pair is two pipes:
//
//	master.Read  ← slave.Write  (runner output; ONLCR: \n → \r\n)
//	master.Write → slave.Read   (keystrokes;    ICRNL: \r → \n)
//
// The input direction is a real [os.Pipe], not an in-memory [io.Pipe]:
// interp.StdIO spawns a stdin-draining copier goroutine for any reader
// that is not an *os.File (subprocesses can only inherit real fds), and
// that copier would race readline for keystrokes and eat them. With a
// real fd the slave input is shared exactly the way the unix PTY slave
// fd is: readline reads it only while a Readline() is pending (the
// ioloop is kick-gated), and foreground commands read it directly in
// between. The slave end exposes the fd via StdinFile for
// newSessionFrom to hand to interp.
//
// The line discipline a kernel PTY would provide is split between the
// far end and readline: the remote terminal (the SSH client that did a
// pty-req, or xterm.js behind /shell) is already in raw mode and does
// the rendering; echo and line editing are readline's own, enabled via
// interactive.Options.AssumeTTY — see the virtualTTY probe in
// Session.Run.
//
// Known gaps vs. a real PTY, all accepted: no SIGWINCH (readline
// re-polls WindowSize per prompt), no $COLUMNS/$LINES updates, no echo
// while a foreground command is reading stdin, `tty` reports "not a
// tty" (subprocess stdin is a pipe fd), and ^C is a byte, not a signal.
//
// The implementation lives in this untagged file so unix test runs
// cover the Windows code path; only the openPTY wiring is build-tagged
// (pty_windows.go).

import (
	"io"
	"os"
	"sync"
)

// vPTY holds the state shared by both ends: the window geometry from
// the SSH pty-req / window-change (or the xterm.js resize message).
type vPTY struct {
	mu   sync.Mutex
	cols int
	rows int
}

func (p *vPTY) setSize(cols, rows uint16) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if cols > 0 {
		p.cols = int(cols)
	}
	if rows > 0 {
		p.rows = int(rows)
	}
}

func (p *vPTY) size() (cols, rows int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cols, p.rows
}

// vptyEnd is one end of the emulated pair. Reads are verbatim; writes
// run through the end's translation (output ONLCR on the slave, input
// ICRNL on the master) before landing in the pipe.
type vptyEnd struct {
	pty       *vPTY
	r         io.ReadCloser
	w         io.WriteCloser
	translate func(b byte) []byte // nil = passthrough
	stdin     *os.File            // slave end only: the real input fd for interp

	closeOnce sync.Once
	closeErr  error
}

// openVPTY builds the emulated master+slave pair, geometry-initialized
// to 80x24 until the first resize.
func openVPTY() (master, slave *vptyEnd, err error) {
	inR, inW, err := os.Pipe() // master → slave (keystrokes); real fds, see package doc
	if err != nil {
		return nil, nil, err
	}
	outR, outW := io.Pipe() // slave → master (output)
	p := &vPTY{cols: 80, rows: 24}
	crlf := []byte("\r\n")
	nl := []byte("\n")
	master = &vptyEnd{pty: p, r: outR, w: inW, translate: func(b byte) []byte {
		if b == '\r' {
			return nl // ICRNL: Enter from a raw terminal is \r
		}
		return nil
	}}
	slave = &vptyEnd{pty: p, r: inR, w: outW, stdin: inR, translate: func(b byte) []byte {
		if b == '\n' {
			return crlf // ONLCR: bare \n stair-steps in a raw terminal
		}
		return nil
	}}
	return master, slave, nil
}

// WindowSize implements virtualTTY (consumed by Session.Run on the slave
// end) so readline reads the geometry the SSH client negotiated.
func (e *vptyEnd) WindowSize() (cols, rows int) { return e.pty.size() }

// StdinFile implements stdinFiler on the slave end: the real fd that
// interp shares with subprocesses without a stdin-draining copier.
func (e *vptyEnd) StdinFile() *os.File { return e.stdin }

func (e *vptyEnd) Read(p []byte) (int, error) { return e.r.Read(p) }

// Write pushes bytes to the peer end, applying the end's byte
// translation. The returned count is in *input* bytes, per io.Writer.
func (e *vptyEnd) Write(p []byte) (int, error) {
	if e.translate == nil {
		return e.w.Write(p)
	}
	var written int
	for len(p) > 0 {
		// Longest run that needs no translation.
		i := 0
		for i < len(p) && e.translate(p[i]) == nil {
			i++
		}
		if i > 0 {
			n, err := e.w.Write(p[:i])
			written += n
			if err != nil {
				return written, err
			}
			p = p[i:]
			continue
		}
		if _, err := e.w.Write(e.translate(p[0])); err != nil {
			return written, err
		}
		written++
		p = p[1:]
	}
	return written, nil
}

// Close tears down this end's halves of both pipes. Closing the master
// unblocks the slave (reads return EOF, writes error) and vice versa —
// the same "peer hung up" semantics the kernel PTY paths rely on for
// teardown. Idempotent: the input halves are real fds, which would
// error on a double close.
func (e *vptyEnd) Close() error {
	e.closeOnce.Do(func() {
		e.closeErr = e.r.Close()
		if werr := e.w.Close(); e.closeErr == nil {
			e.closeErr = werr
		}
	})
	return e.closeErr
}
