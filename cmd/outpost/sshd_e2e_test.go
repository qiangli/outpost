package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/hostauth"
	"github.com/qiangli/outpost/internal/agent/sshclient"
)

// TestSSHDirectE2E drives the full LAN-direct drop-in pair end to end,
// in-process: the `outpost sshd` server half (agent.ServeLANSSH — same
// code path as the standalone `outpost sshd` command and the daemon's
// ssh_listen_addr listener) against the `outpost ssh` client half
// (dialDirectSSH — the path taken by `outpost ssh user@host:2222`).
//
// This is the agentic-bootstrap scenario the feature exists for: an
// agent on one machine reaches an unpaired machine over plain TCP,
// authenticates with the OS password ($OUTPOST_SSH_PASSWORD), TOFU-pins
// the host key, and runs commands — no cloudbox anywhere.
func TestSSHDirectE2E(t *testing.T) {
	tmp := t.TempDir()
	// Confine known_hosts / config to the test dir on every platform
	// UserConfigDir consults.
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	t.Setenv("AppData", filepath.Join(tmp, "appdata"))
	t.Setenv("OUTPOST_SSH_PASSWORD", "secret")

	currentUser, err := hostauth.CurrentUser()
	if err != nil || currentUser == "" {
		t.Fatalf("CurrentUser: %v", err)
	}
	auth := hostauth.StubAuth{Want: map[string]string{currentUser: "secret"}}

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = agent.ServeLANSSH(ctx, ln, agent.Deps{
			Auth:        auth,
			SSHHostKey:  signer,
			SFTPEnabled: true,
		})
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	dialCtx, dcancel := context.WithTimeout(ctx, 15*time.Second)
	defer dcancel()
	cli, cleanup, err := dialDirectSSH(dialCtx, "127.0.0.1", port, currentUser)
	if err != nil {
		t.Fatalf("dialDirectSSH: %v", err)
	}
	defer cleanup()

	// Exec round-trip through the in-process shell on the server side.
	res, err := cli.Exec(ctx, sshclient.ExecOptions{Command: "echo e2e-roundtrip"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit = %d, stderr: %s", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(string(res.Stdout), "e2e-roundtrip") {
		t.Errorf("stdout = %q, want e2e-roundtrip", res.Stdout)
	}

	// Exit codes must propagate openssh-style.
	res, err = cli.Exec(ctx, sshclient.ExecOptions{Command: "exit 7"})
	if err != nil {
		t.Fatalf("Exec exit 7: %v", err)
	}
	if res.ExitCode != 7 {
		t.Errorf("exit = %d, want 7", res.ExitCode)
	}

	// The exec channel runs the embedded-coreutils fallback: `tac` is
	// GNU-only (absent on stock macOS), so on hosts without it this
	// exercises the registry path; where a system tac exists the output
	// contract is identical either way.
	res, err = cli.Exec(ctx, sshclient.ExecOptions{Command: "printf 'a\\nb\\n' | tac"})
	if err != nil {
		t.Fatalf("Exec tac: %v", err)
	}
	if res.ExitCode != 0 || !strings.HasPrefix(string(res.Stdout), "b") {
		t.Errorf("tac exit=%d stdout=%q, want b before a", res.ExitCode, res.Stdout)
	}

	// SFTP subsystem round-trip (what modern scp rides on).
	sf, err := cli.SFTP()
	if err != nil {
		t.Fatalf("SFTP: %v", err)
	}
	remotePath := filepath.Join(tmp, "sftp-roundtrip.txt")
	wf, err := sf.Create(remotePath)
	if err != nil {
		t.Fatalf("sftp create: %v", err)
	}
	if _, err := wf.Write([]byte("via-sftp")); err != nil {
		t.Fatalf("sftp write: %v", err)
	}
	_ = wf.Close()
	got, err := os.ReadFile(remotePath)
	if err != nil || string(got) != "via-sftp" {
		t.Errorf("sftp file = %q err=%v, want via-sftp", got, err)
	}
	_ = sf.Close()

	// TOFU must have pinned the host key — and a second dial must
	// accept the same key silently.
	khPath, err := conf.KnownHostsPath()
	if err != nil {
		t.Fatal(err)
	}
	kh, err := os.ReadFile(khPath)
	if err != nil || !strings.Contains(string(kh), "outpost-127.0.0.1") {
		t.Errorf("known_hosts after TOFU: %q err=%v", kh, err)
	}
	cli2, cleanup2, err := dialDirectSSH(dialCtx, "127.0.0.1", port, currentUser)
	if err != nil {
		t.Fatalf("second dial (pinned key): %v", err)
	}
	cleanup2()
	_ = cli2

	// Wrong password must be rejected by the OS-password gate.
	t.Setenv("OUTPOST_SSH_PASSWORD", "wrong")
	if _, _, err := dialDirectSSH(dialCtx, "127.0.0.1", port, currentUser); err == nil {
		t.Error("dial with wrong password should fail")
	}
}

// TestIsLANAddressLiteral pins the routing rule: `.local` names and IP
// literals dial LAN-direct (drop-in ssh behavior); anything else keeps
// the cloudbox-assisted flow for paired host names.
func TestIsLANAddressLiteral(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"2ivy.local", true},
		{"2IVY.LOCAL", true},
		{"10.0.0.211", true},
		{"::1", true},
		{"fe80::1", true},
		{"2ivy", false},      // paired host name → cloudbox flow
		{"dragon", false},    // paired host name → cloudbox flow
		{"corp.example.com", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isLANAddressLiteral(c.host); got != c.want {
			t.Errorf("isLANAddressLiteral(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}
