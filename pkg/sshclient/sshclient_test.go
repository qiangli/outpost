package sshclient_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"testing"
	"time"

	"github.com/qiangli/outpost/pkg/sshclient"
	"github.com/qiangli/outpost/pkg/sshserver"
	"golang.org/x/crypto/ssh"
)

// Compile-time public API gate: an external consumer can name the client and
// invoke the unattended primitives without importing an internal package.
func TestPublicAPI(t *testing.T) {
	var c *sshclient.Client
	_ = c
	_ = sshclient.Dial
	_ = (*sshclient.Client).Exec
	_ = (*sshclient.Client).SFTP
	_ = (*sshclient.Client).DirectTCPIP
	_ = (*sshclient.Client).LocalForward
	_ = (*sshclient.Client).RemoteForward
	var _ func(context.Context, net.Listener, string, int) error = c.LocalForward
	var _ func(context.Context, string, string) (net.Addr, error) = c.RemoteForward
}

func TestRemoteForwardRoundTrip(t *testing.T) {
	client := newRemoteForwardClient(t)
	defer client.Close()

	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			conn, err := echo.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	remote, err := client.RemoteForward(ctx, "127.0.0.1:0", echo.Addr().String())
	if err != nil {
		t.Fatalf("RemoteForward: %v", err)
	}

	conn, err := net.DialTimeout("tcp", remote.String(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial remote listener %s: %v", remote, err)
	}
	defer conn.Close()
	payload := []byte("remote-forward-round-trip")
	if _, err := conn.Write(payload); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("echo got %q, want %q", got, payload)
	}
}

func TestRemoteForwardCancellation(t *testing.T) {
	client := newRemoteForwardClient(t)
	defer client.Close()

	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := target.Accept()
		if err == nil {
			accepted <- conn
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	remote, err := client.RemoteForward(ctx, "127.0.0.1:0", target.Addr().String())
	if err != nil {
		t.Fatalf("RemoteForward: %v", err)
	}
	forwarded, err := net.DialTimeout("tcp", remote.String(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial remote listener %s: %v", remote, err)
	}
	defer forwarded.Close()
	select {
	case local := <-accepted:
		defer local.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("remote forward did not dial local target")
	}

	cancel()
	_ = forwarded.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := forwarded.Read(make([]byte, 1)); err == nil {
		t.Fatal("active forwarded connection remained open after cancellation")
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, dialErr := net.DialTimeout("tcp", remote.String(), 50*time.Millisecond)
		if conn != nil {
			_ = conn.Close()
		}
		if dialErr != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("remote listener %s still accepted connections after cancellation", remote)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func newRemoteForwardClient(t *testing.T) *sshclient.Client {
	t.Helper()
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
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serverCtx, stopServer := context.WithCancel(context.Background())
	serverErr := make(chan error, 1)
	go func() {
		serverErr <- sshserver.Serve(serverCtx, listener, sshserver.Config{
			AuthorizedKeysPath: keys,
			HostKey:            hostKey,
		})
	}()
	t.Cleanup(func() {
		stopServer()
		_ = listener.Close()
		select {
		case err := <-serverErr:
			if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, net.ErrClosed) {
				t.Errorf("SSH server shutdown: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("SSH server did not stop")
		}
	})

	transport, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	client, err := sshclient.Dial(context.Background(), sshclient.Config{
		Transport:       transport,
		HostAlias:       "remote-forward-test",
		User:            me.Username,
		HostKeyCallback: ssh.FixedHostKey(hostKey.PublicKey()),
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(clientKey)},
	})
	if err != nil {
		t.Fatalf("sshclient.Dial: %v", err)
	}
	return client
}

func newSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}
