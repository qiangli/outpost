package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	sshagent "golang.org/x/crypto/ssh/agent"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/yoke/pkg/sshclient"
)

func directTestKey(t *testing.T) (ed25519.PrivateKey, ssh.Signer) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return key, signer
}

func TestSSHDirectKeyAuth(t *testing.T) {
	for _, tc := range []struct {
		name, file, password                             string
		wrongKey, encrypted, wantFailure                 bool
		agent, emptyAgent, unavailableAgent, changedHost bool
		strict                                           string
	}{
		{name: "key only without TTY", file: "id_ed25519"},
		{name: "second default identity", file: "id_ecdsa"},
		{name: "third default identity", file: "id_rsa"},
		{name: "key precedes password", file: "id_ed25519", password: "wrong"},
		{name: "wrong key without password", file: "id_ed25519", wrongKey: true, wantFailure: true},
		{name: "password after rejected key", file: "id_ed25519", wrongKey: true, password: "secret"},
		{name: "password without keys", password: "secret"},
		{name: "skip encrypted key", file: "id_ed25519", encrypted: true, password: "secret"},
		{name: "agent key only", agent: true},
		{name: "empty agent file fallback", emptyAgent: true, file: "id_ed25519"},
		{name: "unavailable agent file fallback", unavailableAgent: true, file: "id_ed25519"},
		{name: "agent preferred over files", agent: true, file: "id_ed25519", wrongKey: true},
		{name: "unavailable agent password fallback", unavailableAgent: true, password: "secret"},
		{name: "default host key mismatch", file: "id_ed25519", changedHost: true},
		{name: "strict host key mismatch", file: "id_ed25519", changedHost: true, strict: "1"},
		{name: "explicit host key opt out", file: "id_ed25519", changedHost: true, strict: "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("HOME", tmp)
			t.Setenv("USERPROFILE", tmp)
			t.Setenv("XDG_CONFIG_HOME", tmp)
			t.Setenv("AppData", tmp)
			t.Setenv("SSH_AUTH_SOCK", "")
			t.Setenv("OUTPOST_SSH_PASSWORD", tc.password)
			t.Setenv("OUTPOST_SSH_STRICT_HOST_KEY", tc.strict)
			// Make a password prompt fail even when the suite has a TTY.
			oldPrompt := sshPromptPassword
			sshPromptPassword = func(string, bool) (string, error) {
				return "", errors.New("no TTY for password prompt")
			}
			t.Cleanup(func() { sshPromptPassword = oldPrompt })

			key, authorized := directTestKey(t)
			if tc.agent {
				startDirectTestAgent(t, key)
			} else if tc.emptyAgent {
				startDirectTestAgent(t, nil)
			} else if tc.unavailableAgent {
				t.Setenv("SSH_AUTH_SOCK", filepath.Join(tmp, "missing-agent"))
			}
			if tc.wrongKey {
				key, _ = directTestKey(t)
			}
			if tc.file != "" {
				block, err := ssh.MarshalPrivateKey(key, "test")
				if tc.encrypted {
					block, err = ssh.MarshalPrivateKeyWithPassphrase(key, "test", []byte("locked"))
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Join(tmp, ".ssh"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(tmp, ".ssh", tc.file), pem.EncodeToMemory(block), 0600); err != nil {
					t.Fatal(err)
				}
			}

			_, hostKey := directTestKey(t)
			khPath, err := conf.KnownHostsPath()
			if err != nil {
				t.Fatal(err)
			}
			passwordCalls := 0
			cfg := &ssh.ServerConfig{
				PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
					if bytes.Equal(key.Marshal(), authorized.PublicKey().Marshal()) {
						return nil, nil
					}
					return nil, errors.New("unauthorized key")
				},
			}
			if tc.password != "" {
				cfg.PasswordCallback = func(_ ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
					passwordCalls++
					if string(password) == "secret" {
						return nil, nil
					}
					return nil, errors.New("invalid password")
				}
			}
			cfg.AddHostKey(hostKey)
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			if tc.changedHost {
				_, previousKey := directTestKey(t)
				if err := os.MkdirAll(filepath.Dir(khPath), 0700); err != nil {
					t.Fatal(err)
				}
				line := sshclient.HostAliasForHost(ln.Addr().String()) + " " + string(ssh.MarshalAuthorizedKey(previousKey.PublicKey()))
				if err := os.WriteFile(khPath, []byte(line), 0600); err != nil {
					t.Fatal(err)
				}
			}
			defer ln.Close()
			done := make(chan struct{})
			go func() {
				defer close(done)
				conn, err := ln.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				server, _, _, err := ssh.NewServerConn(conn, cfg)
				if err == nil {
					_ = server.Wait()
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, cleanup, err := dialOutpostHost(ctx, "127.0.0.1", "test", ln.Addr().(*net.TCPAddr).Port)
			cleanup()
			<-done
			if tc.changedHost && tc.strict != "0" {
				if err == nil || !strings.Contains(err.Error(), "REMOTE HOST IDENTIFICATION HAS CHANGED") {
					t.Fatalf("changed host key: got %v, want host-key rejection", err)
				}
			} else if tc.wantFailure {
				if err == nil || !strings.Contains(err.Error(), "unable to authenticate") {
					t.Fatalf("wrong key: got %v, want authentication failure", err)
				}
			} else if err != nil {
				t.Fatalf("SSH login: %v", err)
			} else {
				pinned, err := os.ReadFile(khPath)
				if err != nil || !bytes.Contains(pinned, bytes.TrimSpace(ssh.MarshalAuthorizedKey(hostKey.PublicKey()))) {
					t.Fatalf("host key not pinned: %v", err)
				}
			}
			if tc.password == "wrong" && passwordCalls != 0 {
				t.Fatalf("password attempted %d times despite an authorized key", passwordCalls)
			}
		})
	}
}

func startDirectTestAgent(t *testing.T, key ed25519.PrivateKey) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("test agent uses a Unix socket")
	}
	// Keep the socket path below the Unix socket pathname limit on macOS.
	dir, err := os.MkdirTemp("", "ssh-agent-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "agent")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	keyring := sshagent.NewKeyring()
	if key != nil {
		if err := keyring.Add(sshagent.AddedKey{PrivateKey: key}); err != nil {
			_ = ln.Close()
			t.Fatal(err)
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err == nil {
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
			_ = sshagent.ServeAgent(keyring, conn)
		}
	}()
	t.Cleanup(func() { _ = ln.Close(); <-done })
	t.Setenv("SSH_AUTH_SOCK", sock)
}
