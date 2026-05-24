package agent

import (
	"net"
	"os"
	"testing"
)

func TestAgentForward_CloseRemovesSocketAndDir(t *testing.T) {
	// We don't exercise the accept loop here — that path needs a live
	// ssh.ServerConn, which is overkill for a hygiene test. Just
	// poke the struct directly so we know Close() really tears the
	// tempdir down. The accept loop is covered by the end-to-end ssh
	// test (see ssh_test.go) once cloudbox + a paired peer are in
	// the picture.
	dir, err := os.MkdirTemp("", "outpost-sshagent-test-*")
	if err != nil {
		t.Fatalf("mktemp: %v", err)
	}
	sock := dir + "/agent.sock"
	ln, err := net.Listen("unix", sock)
	if err != nil {
		_ = os.RemoveAll(dir)
		t.Fatalf("listen: %v", err)
	}
	af := &agentForward{socketPath: sock, dir: dir, ln: ln}

	if af.SocketPath() != sock {
		t.Errorf("SocketPath()=%q, want %q", af.SocketPath(), sock)
	}
	if _, err := os.Stat(sock); err != nil {
		t.Errorf("socket should exist before Close: %v", err)
	}

	af.Close()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("dir should be gone after Close, stat err: %v", err)
	}

	// Idempotency: second Close must not panic.
	af.Close()

	// Nil receiver is safe.
	var nilAF *agentForward
	nilAF.Close()
	if nilAF.SocketPath() != "" {
		t.Errorf("nil receiver SocketPath should be empty")
	}
}
