package main

// LAN sshd as a service unit: an UNPAIRED daemon serves the sshd unit's
// listener (enabled by default, declared port), and disabling the unit
// withdraws it. This replaces the retired ssh_listen_addr wiring, which
// used `config set --ssh-listen-addr` for the same pre-pairing scenario.

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
// covered in-process by TestSSHDirectE2E and TestSSHDKeyAuthAcceptReject.
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

// waitNoSSHBanner requires that addr accepts no SSH banner within timeout.
func waitNoSSHBanner(t *testing.T, addr string, timeout time.Duration) error {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			_ = c.SetDeadline(time.Now().Add(2 * time.Second))
			line, _ := bufio.NewReader(c).ReadString('\n')
			_ = c.Close()
			if strings.HasPrefix(line, "SSH-2.0-") {
				return fmt.Errorf("unexpected SSH banner on %s: %q", addr, line)
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return nil
}

// TestE2E_FirstRunSSHDUnitServes boots an unpaired daemon, points the
// sshd unit at a free port the documented way (`builtins set
// --service-set` + `restart`), and requires the SSH banner to appear.
// This is the managed-service sshd path: a host that never pairs still
// serves LAN ssh from the daemon.
func TestE2E_FirstRunSSHDUnitServes(t *testing.T) {
	requireE2E(t)
	d := spawnDaemon(t)
	sshPort := freePort(t)
	sshAddr := fmt.Sprintf("127.0.0.1:%d", sshPort)
	d.mustCLI(t, "builtins", "set", "--service-set", fmt.Sprintf("sshd.ports.0.port=%d", sshPort))
	d.mustCLI(t, "restart")
	if err := waitSSHBanner(t, sshAddr, 30*time.Second); err != nil {
		t.Fatalf("unpaired daemon never served ssh on %s: %v\nstdout:\n%s\nstderr:\n%s",
			sshAddr, err, d.stdout.String(), d.stderr.String())
	}
}

// TestE2E_FirstRunSSHDUnitDisableWithdraws pins the off-switch: with the
// unit disabled, a fresh daemon binds no LAN ssh listener.
func TestE2E_FirstRunSSHDUnitDisableWithdraws(t *testing.T) {
	requireE2E(t)
	d := spawnDaemon(t)
	sshPort := freePort(t)
	sshAddr := fmt.Sprintf("127.0.0.1:%d", sshPort)
	d.mustCLI(t, "builtins", "set", "--service-set", fmt.Sprintf("sshd.ports.0.port=%d", sshPort))
	d.mustCLI(t, "builtins", "set", "--service", "sshd=off")
	d.mustCLI(t, "restart")
	// Give the daemon a moment to boot, then require silence.
	time.Sleep(3 * time.Second)
	if err := waitNoSSHBanner(t, sshAddr, 5*time.Second); err != nil {
		t.Fatalf("disabled sshd unit still serves: %v\nstdout:\n%s\nstderr:\n%s",
			err, d.stdout.String(), d.stderr.String())
	}
}
