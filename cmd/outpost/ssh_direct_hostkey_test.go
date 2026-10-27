package main

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/sshclient"
	"golang.org/x/crypto/ssh"
)

func TestSSHDirectHostKeyPinsByPort(t *testing.T) {
	t.Setenv("OUTPOST_SSH_STRICT_HOST_KEY", "")
	path := filepath.Join(t.TempDir(), "known_hosts")
	_, first := directTestKey(t)
	_, second := directTestKey(t)
	for _, host := range []string{"127.0.0.1", "::1"} {
		for _, endpoint := range []struct {
			port string
			key  ssh.Signer
		}{{"22", first}, {"2222", second}} {
			alias := sshclient.HostAliasForHost(net.JoinHostPort(host, endpoint.port))
			cb, err := sshDirectHostKeyCallback(path, alias)
			if err != nil {
				t.Fatal(err)
			}
			if err := cb("", nil, endpoint.key.PublicKey()); err != nil {
				t.Fatalf("pin %s: %v", alias, err)
			}
		}
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"127.0.0.1", "::1"} {
		for _, endpoint := range []struct {
			port       string
			key, wrong ssh.Signer
		}{{"22", first, second}, {"2222", second, first}} {
			alias := sshclient.HostAliasForHost(net.JoinHostPort(host, endpoint.port))
			cb, err := sshDirectHostKeyCallback(path, alias)
			if err != nil {
				t.Fatal(err)
			}
			if err := cb("", nil, endpoint.key.PublicKey()); err != nil {
				t.Fatalf("verify %s: %v", alias, err)
			}
			if err := cb("", nil, endpoint.wrong.PublicKey()); err == nil || !strings.Contains(err.Error(), "REMOTE HOST IDENTIFICATION HAS CHANGED") {
				t.Fatalf("changed key for %s accepted: %v", alias, err)
			}
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("verification changed pins: %v", err)
	}
}
