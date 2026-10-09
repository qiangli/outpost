package main

// O4 managed-service sshd regression tests (Story 1537): an UNPAIRED
// daemon with an explicit ssh_listen_addr must mount the LAN SSH
// listener. The first-run early return in `start` used to exit before
// the listener block, so the setting was silently dead until pairing.

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

// waitSSHBanner waits until addr completes an SSH handshake preamble
// (the "SSH-2.0-..." banner, sent before any authentication) or the
// timeout expires. A banner proves the listener serves; auth itself is
// covered in-process by TestSSHDirectE2E.
func waitSSHBanner(t *testing.T, addr string, timeout time.Duration) error {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		line, err := bufio.NewReader(c).ReadString('\n')
		_ = c.Close()
		if err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		if strings.HasPrefix(line, "SSH-2.0-") {
			return nil
		}
		return fmt.Errorf("unexpected banner %q", line)
	}
	return fmt.Errorf("no SSH banner on %s within %s", addr, timeout)
}

// TestE2E_FirstRunLANSSHListener boots an unpaired daemon, sets
// ssh_listen_addr the documented way (`config set` + `restart`), and
// requires the SSH banner to appear. This is the managed-service sshd
// path `bashy self install --service` leaves behind on a host that
// never pairs.
func TestE2E_FirstRunLANSSHListener(t *testing.T) {
	requireE2E(t)
	d := spawnDaemon(t)
	sshPort := freePort(t)
	sshAddr := fmt.Sprintf("127.0.0.1:%d", sshPort)
	d.mustCLI(t, "config", "set", "--ssh-listen-addr", sshAddr)
	d.mustCLI(t, "restart")
	if err := waitSSHBanner(t, sshAddr, 30*time.Second); err != nil {
		t.Fatalf("unpaired daemon never served ssh on %s: %v\nstdout:\n%s\nstderr:\n%s",
			sshAddr, err, d.stdout.String(), d.stderr.String())
	}
}

// TestE2E_FirstRunNoLANSSHByDefault pins the other half of the
// contract: without an explicit ssh_listen_addr a fresh daemon binds
// nothing extra. The fix must stay default-safe.
func TestE2E_FirstRunNoLANSSHByDefault(t *testing.T) {
	requireE2E(t)
	sshPort := freePort(t)
	sshAddr := fmt.Sprintf("127.0.0.1:%d", sshPort)
	d := spawnDaemon(t)
	_ = d
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", sshAddr, 500*time.Millisecond)
		if err == nil {
			_ = c.Close()
			t.Fatalf("fresh daemon listens on %s with no ssh_listen_addr set", sshAddr)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
