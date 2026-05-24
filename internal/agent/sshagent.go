package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/crypto/ssh"
)

// agentForward holds the per-session state for `ssh -A` (auth-agent
// forwarding). A single Unix socket lives in a 0700 tempdir; each
// accepted connection opens an `auth-agent@openssh.com` channel back
// to the client and byte-bridges. SocketPath is what gets stamped into
// SSH_AUTH_SOCK for the runner.
//
// Lifetime is per-SSH-session-channel. handleSSHSession defers
// teardown when the channel ends so the tempdir always goes away.
type agentForward struct {
	socketPath string
	dir        string

	mu     sync.Mutex
	closed bool
	ln     net.Listener
}

// startAgentForward creates the per-session socket and starts the
// accept loop that pushes traffic back to the SSH client. The socket
// path is returned via af.SocketPath for the caller to stamp into the
// runner env. Returns nil + error on any setup failure — caller treats
// that as auth-agent-req failure (reply false).
func startAgentForward(ctx context.Context, sc *ssh.ServerConn) (*agentForward, error) {
	dir, err := os.MkdirTemp("", "outpost-sshagent-*")
	if err != nil {
		return nil, fmt.Errorf("agent-forward: mktemp: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("agent-forward: chmod dir: %w", err)
	}
	sockPath := filepath.Join(dir, "agent.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("agent-forward: listen: %w", err)
	}
	// Socket perms: 0600. The SSH OS-user gate already proves identity;
	// this just keeps other local users on the box from poking the
	// forwarded agent through the loopback fs. (Unix-socket perms are
	// honored by the kernel only on Linux/macOS — fine here, the
	// outpost itself doesn't run on Windows in any deployment that
	// uses SSH agent forwarding.)
	if err := os.Chmod(sockPath, 0o600); err != nil {
		_ = ln.Close()
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("agent-forward: chmod sock: %w", err)
	}

	af := &agentForward{socketPath: sockPath, dir: dir, ln: ln}
	go af.acceptLoop(ctx, sc)
	return af, nil
}

func (af *agentForward) acceptLoop(ctx context.Context, sc *ssh.ServerConn) {
	for {
		c, err := af.ln.Accept()
		if err != nil {
			return // listener closed → exit
		}
		go bridgeAuthAgentChannel(ctx, sc, c)
	}
}

// Close tears the forward down. Idempotent and safe from multiple
// goroutines — the SSH session's defer and an explicit cancel path can
// both fire.
func (af *agentForward) Close() {
	if af == nil {
		return
	}
	af.mu.Lock()
	defer af.mu.Unlock()
	if af.closed {
		return
	}
	af.closed = true
	if af.ln != nil {
		_ = af.ln.Close()
	}
	if af.dir != "" {
		_ = os.RemoveAll(af.dir)
	}
}

// SocketPath returns the Unix socket path to stamp into SSH_AUTH_SOCK.
// Nil receiver returns "" so callers can pass an absent forward
// through without branching.
func (af *agentForward) SocketPath() string {
	if af == nil {
		return ""
	}
	return af.socketPath
}

// bridgeAuthAgentChannel is the per-accepted-connection bridge: open
// an `auth-agent@openssh.com` channel back to the client, byte-bridge
// it to the accepted Unix-socket connection, tear down when either
// side closes. Mirrors handleDirectTCPIP's io.Copy pattern; the SSH
// auth-agent protocol is opaque to the bridge so we don't need to
// parse anything.
func bridgeAuthAgentChannel(ctx context.Context, sc *ssh.ServerConn, c net.Conn) {
	_ = ctx
	defer c.Close()
	ch, chReqs, err := sc.OpenChannel("auth-agent@openssh.com", nil)
	if err != nil {
		slog.Info("auth-agent: client refused channel", "err", err)
		return
	}
	defer ch.Close()
	// auth-agent channels don't carry channel requests — drain so the
	// crypto/ssh request goroutine doesn't pile up against unread reqs.
	go ssh.DiscardRequests(chReqs)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(c, ch)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(ch, c)
	}()
	wg.Wait()
}
