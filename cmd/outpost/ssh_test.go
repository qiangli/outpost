package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestPipeSSHProxy_StdinEOFDoesNotTerminate is the regression test
// for the symptom that motivated this fix: under certain parent
// shells the local stdin returns EOF early (before SSH actually
// finishes), and the old "first-side-to-end terminates" design
// closed the WS mid-handshake. After the fix, only the server side
// closing should terminate; an early stdin EOF must allow inbound
// server bytes to keep arriving on stdout.
func TestPipeSSHProxy_StdinEOFDoesNotTerminate(t *testing.T) {
	// WS handler that sends a fixed payload then keeps the conn open
	// until the test releases it.
	released := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		// Delay so that the client's stdin EOF is observed BEFORE
		// any server-side bytes hit the WS. This makes the race
		// deterministic: under the old "first-side-to-end terminates"
		// design, the WS is closed during this sleep and the banner
		// below never reaches the client's stdout.
		time.Sleep(100 * time.Millisecond)
		nc := websocket.NetConn(r.Context(), c, websocket.MessageBinary)
		if _, err := nc.Write([]byte("SSH-2.0-Go\r\n")); err != nil {
			t.Errorf("write banner: %v", err)
			return
		}
		<-released
	}))
	t.Cleanup(srv.Close)

	wsURL := strings.Replace(srv.URL, "http://", "ws://", 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	nc := websocket.NetConn(ctx, c, websocket.MessageBinary)

	// Stdin EOFs immediately — this is the parent-shell quirk we're
	// guarding against. Stdout collects whatever the server sends.
	stdin := strings.NewReader("")
	var stdout bytes.Buffer

	done := make(chan error, 1)
	go func() {
		done <- pipeSSHProxy(ctx, c, nc, stdin, &stdout)
	}()

	// Allow time for the server's banner to round-trip onto stdout.
	// Poll instead of a fixed sleep: catches both fast and slow CI.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.HasPrefix(stdout.String(), "SSH-2.0-Go") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if got := stdout.String(); !strings.HasPrefix(got, "SSH-2.0-Go") {
		// Buffer should contain the banner. If empty, the old bug
		// (WS closed on stdin EOF before server bytes drained) is
		// back.
		t.Fatalf("stdout missing server banner — got %q", got)
	}

	// Session must still be alive. Releasing the server now should
	// close the WS, which in turn should terminate the pipe.
	close(released)

	select {
	case err := <-done:
		// EOF / normal closure is the success signal.
		if err != nil && !isExpectedClose(err) {
			t.Fatalf("pipe returned unexpected error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pipeSSHProxy did not exit after server closed WS")
	}
}

// TestPipeSSHProxy_ServerCloseTerminates confirms that the server
// closing the WS terminates the pipe — the authoritative "session
// over" signal must work, otherwise the proxy would hang forever.
func TestPipeSSHProxy_ServerCloseTerminates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		// Close immediately — emulates the server-side hanging up
		// the SSH session.
		c.Close(websocket.StatusNormalClosure, "bye")
	}))
	t.Cleanup(srv.Close)

	wsURL := strings.Replace(srv.URL, "http://", "ws://", 1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	nc := websocket.NetConn(ctx, c, websocket.MessageBinary)

	// Pipe a slow writer that's still going when the server closes.
	// pipeSSHProxy must not wait on this — server-close is
	// authoritative.
	slowReader, slowWriter := io.Pipe()
	t.Cleanup(func() { _ = slowWriter.Close() })

	done := make(chan error, 1)
	go func() {
		done <- pipeSSHProxy(ctx, c, nc, slowReader, io.Discard)
	}()

	select {
	case err := <-done:
		if err != nil && !isExpectedClose(err) {
			t.Fatalf("pipe returned unexpected error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pipeSSHProxy did not exit after server-initiated close")
	}
}

// isExpectedClose recognizes the family of errors that count as a
// clean session end: EOF, websocket close frames, net "use of
// closed network connection."
func isExpectedClose(err error) bool {
	if err == nil {
		return true
	}
	if err == io.EOF {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "EOF") ||
		strings.Contains(s, "closed") ||
		strings.Contains(s, "StatusNormalClosure")
}
