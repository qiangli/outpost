package sshserver_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/qiangli/outpost/pkg/sshserver"
)

func TestServeKeyAuthExecSFTPAndForward(t *testing.T) {
	me, err := user.Current()
	if err != nil || me.Username == "" {
		t.Skipf("current user unavailable: %v", err)
	}
	hostKey := newSigner(t)
	clientKey := newSigner(t)
	keys := filepath.Join(t.TempDir(), "authorized_keys")
	if err := os.WriteFile(keys, ssh.MarshalAuthorizedKey(clientKey.PublicKey()), 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- sshserver.Serve(ctx, ln, sshserver.Config{
			AuthorizedKeysPath: keys,
			HostKey:            hostKey,
		})
	}()

	client := dial(t, ln.Addr().String(), me.Username, clientKey, hostKey)
	defer client.Close()

	sess, err := client.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	out, err := sess.Output("printf sshserver-exec")
	_ = sess.Close()
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if string(out) != "sshserver-exec" {
		t.Fatalf("exec output %q", out)
	}

	sftpClient, err := sftp.NewClient(client)
	if err != nil {
		t.Fatalf("sftp: %v", err)
	}
	remote := filepath.Join(t.TempDir(), "roundtrip.txt")
	f, err := sftpClient.Create(remote)
	if err != nil {
		t.Fatalf("sftp create: %v", err)
	}
	if _, err := f.Write([]byte("via-sftp")); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	f, err = sftpClient.Open(remote)
	if err != nil {
		t.Fatalf("sftp open: %v", err)
	}
	got, err := io.ReadAll(f)
	_ = f.Close()
	_ = sftpClient.Close()
	if err != nil || string(got) != "via-sftp" {
		t.Fatalf("sftp read %q: %v", got, err)
	}

	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	go func() {
		conn, err := target.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, len("forwarded"))
		if _, err := io.ReadFull(conn, buf); err == nil {
			_, _ = conn.Write(buf)
		}
	}()
	forward, err := client.Dial("tcp", target.Addr().String())
	if err != nil {
		t.Fatalf("direct-tcpip: %v", err)
	}
	if _, err := forward.Write([]byte("forwarded")); err != nil {
		t.Fatal(err)
	}
	forwarded := make([]byte, len("forwarded"))
	_, err = io.ReadFull(forward, forwarded)
	_ = forward.Close()
	if err != nil {
		t.Fatalf("forward read: %v", err)
	}
	if string(forwarded) != "forwarded" {
		t.Fatalf("forward output %q", forwarded)
	}

	cancel()
	select {
	case err := <-serverErr:
		if err != context.Canceled && err != net.ErrClosed {
			t.Fatalf("shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not shut down")
	}
}

func TestServeEnforcesSameUser(t *testing.T) {
	me, err := user.Current()
	if err != nil || me.Username == "" {
		t.Skipf("current user unavailable: %v", err)
	}
	hostKey := newSigner(t)
	clientKey := newSigner(t)
	keys := filepath.Join(t.TempDir(), "authorized_keys")
	if err := os.WriteFile(keys, ssh.MarshalAuthorizedKey(clientKey.PublicKey()), 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = sshserver.Serve(ctx, ln, sshserver.Config{AuthorizedKeysPath: keys, HostKey: hostKey}) }()
	client, err := ssh.Dial("tcp", ln.Addr().String(), &ssh.ClientConfig{
		User: "definitely-not-the-current-user", Auth: []ssh.AuthMethod{ssh.PublicKeys(clientKey)},
		HostKeyCallback: ssh.FixedHostKey(hostKey.PublicKey()), Timeout: 2 * time.Second,
	})
	if client != nil {
		_ = client.Close()
	}
	if err == nil {
		t.Fatal("different user accepted")
	}
}

func newSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func dial(t *testing.T, address, username string, key, hostKey ssh.Signer) *ssh.Client {
	t.Helper()
	client, err := ssh.Dial("tcp", address, &ssh.ClientConfig{
		User: username, Auth: []ssh.AuthMethod{ssh.PublicKeys(key)},
		HostKeyCallback: ssh.FixedHostKey(hostKey.PublicKey()), Timeout: 3 * time.Second,
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return client
}
