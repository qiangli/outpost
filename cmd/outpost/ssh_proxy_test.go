// ssh-proxy direct-first regression tests (Sprint 490 story 09d782295a0d).
//
// Operator rule: when the user's own keys reach the host over the LAN,
// ssh-proxy must use that direct TCP path and never touch the
// cloudbox relay/elevation. Only when no direct path connects may it
// fall back to the relay — and headless (no TTY) with neither path
// available must fail fast with one error naming both attempts,
// never prompting for a password.
package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// stubSSHProxySeams replaces the direct-dial and relay legs for one
// test and restores them afterwards. Pass nil to keep the real leg.
func stubSSHProxySeams(t *testing.T, dial func(ctx context.Context, candidates []string, timeout time.Duration) (net.Conn, error), relay func(ctx context.Context, host string, stdin io.Reader, stdout io.Writer) error) {
	t.Helper()
	t.Cleanup(func() {
		sshProxyDialDirect = dialSSHProxyDirect
		sshProxyRunRelay = runSSHProxyRelay
	})
	if dial != nil {
		sshProxyDialDirect = dial
	}
	if relay != nil {
		sshProxyRunRelay = relay
	}
}

// closedLoopbackPort returns a 127.0.0.1 port with nothing listening
// (bind then close), so direct dials fail fast with refused.
func closedLoopbackPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func TestSSHProxyCandidates(t *testing.T) {
	cases := []struct {
		name string
		host string
		port int
		want []string
	}{
		{"bare host probes 22 then 2222 with mdns alias", "testhost", 0,
			[]string{"testhost:22", "testhost.local:22", "testhost:2222", "testhost.local:2222"}},
		{"explicit port keeps host and mdns alias", "testhost", 2222,
			[]string{"testhost:2222", "testhost.local:2222"}},
		{"local-suffixed host is not duplicated", "testhost.local", 0,
			[]string{"testhost.local:22", "testhost.local:2222"}},
		{"ip literal never gains an mdns alias", "127.0.0.1", 0,
			[]string{"127.0.0.1:22", "127.0.0.1:2222"}},
		{"ip literal with explicit port is a single candidate", "127.0.0.1", 9999,
			[]string{"127.0.0.1:9999"}},
		{"host already carrying a port is used as-is", "testhost:2222", 0,
			[]string{"testhost:2222"}},
		{"empty host has no candidates", "", 0, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := sshProxyCandidates(c.host, c.port)
			if len(got) != len(c.want) {
				t.Fatalf("sshProxyCandidates(%q, %d) = %v, want %v", c.host, c.port, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("sshProxyCandidates(%q, %d) = %v, want %v", c.host, c.port, got, c.want)
				}
			}
		})
	}
}

func TestSSHProxyModeResolution(t *testing.T) {
	t.Setenv("OUTPOST_SSH_PROXY_MODE", "")
	if got := sshProxyMode(""); got != "auto" {
		t.Fatalf("empty flag/env = %q, want auto", got)
	}
	if got := sshProxyMode("direct"); got != "direct" {
		t.Fatalf("flag direct = %q", got)
	}
	t.Setenv("OUTPOST_SSH_PROXY_MODE", "relay")
	if got := sshProxyMode(""); got != "relay" {
		t.Fatalf("env relay = %q", got)
	}
	if got := sshProxyMode("direct"); got != "direct" {
		t.Fatalf("flag must beat env, got %q", got)
	}
	t.Setenv("OUTPOST_SSH_PROXY_MODE", "bogus")
	if got := sshProxyMode(""); got != "auto" {
		t.Fatalf("bogus env must fall back to auto, got %q", got)
	}
}

func TestSSHProxyDirectPortResolution(t *testing.T) {
	t.Setenv("OUTPOST_SSH_DIRECT_PORT", "")
	if got := sshProxyDirectPort(0); got != 0 {
		t.Fatalf("unset = %d, want 0 (probe defaults)", got)
	}
	t.Setenv("OUTPOST_SSH_DIRECT_PORT", "2222")
	if got := sshProxyDirectPort(0); got != 2222 {
		t.Fatalf("env 2222 = %d", got)
	}
	if got := sshProxyDirectPort(9999); got != 9999 {
		t.Fatalf("flag must beat env, got %d", got)
	}
	t.Setenv("OUTPOST_SSH_DIRECT_PORT", "bogus")
	if got := sshProxyDirectPort(0); got != 0 {
		t.Fatalf("bogus env must fall back to 0, got %d", got)
	}
}

// TestSSHProxyDirectFirstUsesLAN is the story's core: a reachable
// LAN sshd-like listener wins and the relay leg never runs.
func TestSSHProxyDirectFirstUsesLAN(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	saw := make(chan string, 1)
	go func() {
		c, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 64)
		_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, _ := c.Read(buf)
		saw <- string(buf[:n])
		_, _ = c.Write([]byte("lan-ok"))
	}()

	var relayCalls atomic.Int32
	stubSSHProxySeams(t, nil, func(ctx context.Context, host string, stdin io.Reader, stdout io.Writer) error {
		relayCalls.Add(1)
		return errors.New("relay must not run when the LAN path works")
	})

	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := runSSHProxyWithIO(ctx, "127.0.0.1", sshProxyOptions{directPort: port}, strings.NewReader("hello-lan"), &out); err != nil {
		t.Fatalf("direct-first proxy over LAN listener: %v", err)
	}
	if out.String() != "lan-ok" {
		t.Fatalf("stdout via LAN = %q, want %q", out.String(), "lan-ok")
	}
	select {
	case got := <-saw:
		if got != "hello-lan" {
			t.Fatalf("listener saw %q, want %q", got, "hello-lan")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("listener never received the stdin bytes")
	}
	if n := relayCalls.Load(); n != 0 {
		t.Fatalf("relay ran %d times, want 0", n)
	}
}

// TestSSHProxyDirectFirstFallsBackToRelay: nothing on the LAN, so
// the existing relay leg must run (unchanged behavior).
func TestSSHProxyDirectFirstFallsBackToRelay(t *testing.T) {
	port := closedLoopbackPort(t)
	var relayCalls atomic.Int32
	stubSSHProxySeams(t, nil, func(ctx context.Context, host string, stdin io.Reader, stdout io.Writer) error {
		relayCalls.Add(1)
		_, _ = stdout.Write([]byte("via-relay"))
		return nil
	})

	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := runSSHProxyWithIO(ctx, "127.0.0.1", sshProxyOptions{directPort: port}, strings.NewReader(""), &out); err != nil {
		t.Fatalf("relay fallback: %v", err)
	}
	if out.String() != "via-relay" {
		t.Fatalf("stdout = %q, want relay output", out.String())
	}
	if n := relayCalls.Load(); n != 1 {
		t.Fatalf("relay ran %d times, want 1", n)
	}
}

// TestSSHProxyDirectFirstNeitherFailsFastNoPrompt: no LAN path and a
// relay that would need elevation (no TTY) must surface one error
// naming both attempts — and return, never block on a prompt.
func TestSSHProxyDirectFirstNeitherFailsFastNoPrompt(t *testing.T) {
	port := closedLoopbackPort(t)
	var prompted atomic.Int32
	stubSSHProxySeams(t, nil, func(ctx context.Context, host string, stdin io.Reader, stdout io.Writer) error {
		// The real relay refuses without a TTY instead of prompting;
		// the stub models that refusal without any prompt surface.
		if haveTTY() {
			prompted.Add(1)
		}
		return errors.New("re-elevate testhost: no TTY for password prompt")
	})

	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	start := time.Now()
	err := runSSHProxyWithIO(ctx, "127.0.0.1", sshProxyOptions{directPort: port}, strings.NewReader(""), &out)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected an error when neither path exists, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "direct") || !strings.Contains(msg, "relay") {
		t.Fatalf("error must name both attempts, got: %v", err)
	}
	if !strings.Contains(msg, "no TTY") {
		t.Fatalf("error must surface the headless relay refusal, got: %v", err)
	}
	if prompted.Load() != 0 {
		t.Fatal("relay leg prompted despite no TTY")
	}
	t.Logf("neither-path error returned in %s", elapsed)
}

func TestSSHProxyModeForceRelaySkipsDirect(t *testing.T) {
	var dialCalls atomic.Int32
	stubSSHProxySeams(t,
		func(ctx context.Context, candidates []string, timeout time.Duration) (net.Conn, error) {
			dialCalls.Add(1)
			return nil, errors.New("direct must not be attempted in relay mode")
		},
		func(ctx context.Context, host string, stdin io.Reader, stdout io.Writer) error {
			return nil
		},
	)
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := runSSHProxyWithIO(ctx, "testhost", sshProxyOptions{mode: "relay"}, strings.NewReader(""), &out); err != nil {
		t.Fatalf("relay mode: %v", err)
	}
	if n := dialCalls.Load(); n != 0 {
		t.Fatalf("direct dial ran %d times in relay mode, want 0", n)
	}
}

func TestSSHProxyModeForceDirectSkipsRelay(t *testing.T) {
	port := closedLoopbackPort(t)
	var relayCalls atomic.Int32
	stubSSHProxySeams(t, nil, func(ctx context.Context, host string, stdin io.Reader, stdout io.Writer) error {
		relayCalls.Add(1)
		return nil
	})
	var out bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := runSSHProxyWithIO(ctx, "127.0.0.1", sshProxyOptions{mode: "direct", directPort: port}, strings.NewReader(""), &out)
	if err == nil {
		t.Fatal("expected direct-mode error when the LAN path is absent, got nil")
	}
	if !strings.Contains(err.Error(), "direct") {
		t.Fatalf("direct-mode error must name the direct attempt, got: %v", err)
	}
	if n := relayCalls.Load(); n != 0 {
		t.Fatalf("relay ran %d times in direct mode, want 0", n)
	}
}
