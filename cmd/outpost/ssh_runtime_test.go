package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/outpost/internal/agent/admincore"
	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/discovery"
)

// splitAdHocHostPort feeds the `outpost ssh user@host[:port]` direct-
// dial decision: a parsed port forces the plain-TCP LAN path, no port
// keeps the name on the cloudbox-assisted flow. IPv6 literals must
// not have their colons mistaken for a port separator.
func TestSplitAdHocHostPort(t *testing.T) {
	cases := []struct {
		in       string
		wantHost string
		wantPort int
	}{
		{"host-c", "host-c", 0},
		{"192.168.1.5", "192.168.1.5", 0},
		{"192.168.1.5:2222", "192.168.1.5", 2222},
		{"host.local:22", "host.local", 22},
		{" host.local:2222 ", "host.local", 2222},
		{"[::1]:2222", "::1", 2222},
		{"::1", "::1", 0},         // bare IPv6 — colons are address bytes
		{"fe80::1", "fe80::1", 0}, // bare IPv6, multiple colons
		{"host:notaport", "host:notaport", 0},
		{"host:70000", "host:70000", 0}, // out of range → not a port
		{"host:", "host:", 0},           // empty port
		{":2222", ":2222", 0},           // no host — not an ad-hoc target
	}
	for _, tc := range cases {
		host, port := splitAdHocHostPort(tc.in)
		if host != tc.wantHost || port != tc.wantPort {
			t.Errorf("splitAdHocHostPort(%q) = (%q, %d), want (%q, %d)",
				tc.in, host, port, tc.wantHost, tc.wantPort)
		}
	}
}

func TestDialMeshDirectUnpublishedPeerSkipsForwardAndTicket(t *testing.T) {
	var ticketCalls int
	ticketServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ticketCalls++
		t.Error("unpublished peer must not request a ticket")
	}))
	defer ticketServer.Close()

	fc := meshTestConfig(t, ticketServer.URL)
	toolCalls := []string{}
	stubMeshToolRunner(t, func(_ context.Context, name string, _ any, out any) error {
		toolCalls = append(toolCalls, name)
		switch name {
		case "outpost_mesh_link":
			meshToolResult(t, out, map[string]any{"link": admincore.MeshHostLinkView{
				Found: true, Direct: true, PeerID: "peer-target",
			}})
		case "outpost_mesh_resolve":
			meshToolResult(t, out, map[string]any{"peers": []admincore.MeshResolvedPeer{
				{Host: "another-host", PeerID: "peer-other"},
			}})
		default:
			t.Fatalf("unexpected mesh tool call %q", name)
		}
		return nil
	})

	_, _, err := dialMeshDirect(context.Background(), fc, "bearer", "target", "user", "cookie")
	if !errors.Is(err, errMeshNotAvailable) {
		t.Fatalf("dialMeshDirect error = %v, want errMeshNotAvailable", err)
	}
	if got, want := strings.Join(toolCalls, ","), "outpost_mesh_link,outpost_mesh_resolve"; got != want {
		t.Fatalf("mesh tool calls = %q, want %q (no forward)", got, want)
	}
	if ticketCalls != 0 {
		t.Fatalf("peer-ticket calls = %d, want 0", ticketCalls)
	}
}

func TestDialMeshDirectPublishedPeerOpensForwardAndRequestsTicket(t *testing.T) {
	var ticketCalls int
	ticketServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ticketCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ticket":"test-ticket"}`))
	}))
	defer ticketServer.Close()

	fc := meshTestConfig(t, ticketServer.URL)
	toolCalls := []string{}
	stubMeshToolRunner(t, func(_ context.Context, name string, _ any, out any) error {
		toolCalls = append(toolCalls, name)
		switch name {
		case "outpost_mesh_link":
			meshToolResult(t, out, map[string]any{"link": admincore.MeshHostLinkView{
				Found: true, Direct: true, PeerID: "peer-target",
			}})
		case "outpost_mesh_resolve":
			meshToolResult(t, out, map[string]any{"peers": []admincore.MeshResolvedPeer{
				{Host: "target", PeerID: "peer-target"},
			}})
		case "outpost_mesh_listen":
			// Nothing is listening here: the test needs only to verify that a
			// published peer commits to the direct path before its WS dial.
			meshToolResult(t, out, map[string]any{"addr": "127.0.0.1:1"})
		case "outpost_mesh_close_listen":
		default:
			t.Fatalf("unexpected mesh tool call %q", name)
		}
		return nil
	})

	_, _, err := dialMeshDirect(context.Background(), fc, "bearer", "target", "user", "cookie")
	if err == nil || errors.Is(err, errMeshNotAvailable) {
		t.Fatalf("dialMeshDirect error = %v, want attempted mesh WS dial error", err)
	}
	if got, want := strings.Join(toolCalls, ","), "outpost_mesh_link,outpost_mesh_resolve,outpost_mesh_listen,outpost_mesh_close_listen"; got != want {
		t.Fatalf("mesh tool calls = %q, want %q", got, want)
	}
	if ticketCalls != 1 {
		t.Fatalf("peer-ticket calls = %d, want 1", ticketCalls)
	}
}

func stubMeshToolRunner(t *testing.T, fn func(context.Context, string, any, any) error) {
	t.Helper()
	old := meshToolRunner
	meshToolRunner = fn
	t.Cleanup(func() { meshToolRunner = old })
}

func meshToolResult(t *testing.T, out, value any) {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal mesh result: %v", err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatalf("unmarshal mesh result: %v", err)
	}
}

func meshTestConfig(t *testing.T, rawURL string) *conf.FileConfig {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse ticket server URL: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse ticket server port: %v", err)
	}
	return &conf.FileConfig{ServerAddr: u.Hostname(), ServerPort: port, Protocol: "ws"}
}

// An explicit OUTPOST_SSH_PASSWORD is non-interactive: after the server
// rejects it, the callback must fail rather than fall back to a /dev/tty
// prompt (which blocks forever when the caller runs under a PTY).
func TestSSHPasswordCallbackEnvRejectionNeverPrompts(t *testing.T) {
	t.Setenv("OUTPOST_SSH_PASSWORD", "wrong")
	prev := sshPromptPassword
	sshPromptPassword = func(string, bool) (string, error) {
		t.Fatal("interactive prompt reached with OUTPOST_SSH_PASSWORD set")
		return "", nil
	}
	defer func() { sshPromptPassword = prev }()

	cb := sshPasswordCallback("u", "h")
	if pw, err := cb(); err != nil || pw != "wrong" {
		t.Fatalf("first attempt = %q, %v; want env password", pw, err)
	}
	for i := 0; i < 2; i++ {
		if _, err := cb(); err == nil || !strings.Contains(err.Error(), "OUTPOST_SSH_PASSWORD was rejected") {
			t.Fatalf("retry %d err = %v; want env rejection", i, err)
		}
	}
}

// stubLANPeerLookup replaces the mDNS lookup seam for the test.
func stubLANPeerLookup(t *testing.T, peer *discovery.Peer, err error) {
	t.Helper()
	old := lanPeerLookup
	lanPeerLookup = func(context.Context, string) (*discovery.Peer, error) {
		return peer, err
	}
	t.Cleanup(func() { lanPeerLookup = old })
}

// blackholeListener accepts loopback TCP connections and holds them open
// without sending a byte — the shape of a dead endpoint that would hang
// an unbounded dial forever.
func blackholeListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("blackhole listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				// Hold until the test closes the listener; never
				// reply so the dialer must hit its own budget.
				time.Sleep(30 * time.Second)
				_ = c.Close()
			}()
		}
	}()
	return ln.Addr().String()
}

// shrinkRungTimeouts cuts every paired-rung network budget for the test.
func shrinkRungTimeouts(t *testing.T, d time.Duration) {
	t.Helper()
	oldLAN, oldLANHS := sshLANDialTimeout, sshLANHandshakeTimeout
	oldMesh, oldMeshHS := sshMeshDialTimeout, sshMeshHandshakeTimeout
	oldTun, oldTunHS := sshTunnelDialTimeout, sshTunnelHandshakeTimeout
	sshLANDialTimeout, sshLANHandshakeTimeout = d, d
	sshMeshDialTimeout, sshMeshHandshakeTimeout = d, d
	sshTunnelDialTimeout, sshTunnelHandshakeTimeout = d, d
	t.Cleanup(func() {
		sshLANDialTimeout, sshLANHandshakeTimeout = oldLAN, oldLANHS
		sshMeshDialTimeout, sshMeshHandshakeTimeout = oldMesh, oldMeshHS
		sshTunnelDialTimeout, sshTunnelHandshakeTimeout = oldTun, oldTunHS
	})
}

// isolatedHome points HOME/XDG dirs at scratch space so dial tests never
// touch the operator's real paired config or known_hosts.
func isolatedHome(t *testing.T) {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", tmp)
	t.Setenv("XDG_CACHE_HOME", tmp)
}

func ticketStubServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ticket":"test-ticket"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A blackholed LAN endpoint must fail inside the rung budget so the dial
// falls through to the next rung instead of hanging the command.
func TestDialLANDirectBlackholeFallsThroughWithinBudget(t *testing.T) {
	isolatedHome(t)
	shrinkRungTimeouts(t, time.Second)
	addr := blackholeListener(t)
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split blackhole addr: %v", err)
	}
	port, _ := strconv.Atoi(portStr)
	stubLANPeerLookup(t, &discovery.Peer{
		AgentName: "target",
		ID:        "test-fingerprint",
		Endpoints: []discovery.Endpoint{
			{Kind: discovery.EndpointLANSSHWS, Host: host, Port: port},
		},
	}, nil)
	fc := meshTestConfig(t, ticketStubServer(t).URL)

	start := time.Now()
	_, _, err = dialLANDirect(context.Background(), fc, "bearer", "target", "user", "cookie")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("dialLANDirect to blackhole succeeded, want error")
	}
	if errors.Is(err, errLANNotAvailable) {
		t.Fatalf("dialLANDirect error = %v, want an attempted-dial error", err)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("dialLANDirect blackhole took %v, want fall-through inside the rung budget", elapsed)
	}
}

// Same contract for the mesh rung: a dead forward must fail fast.
func TestDialMeshDirectBlackholeFallsThroughWithinBudget(t *testing.T) {
	isolatedHome(t)
	shrinkRungTimeouts(t, time.Second)
	fwdAddr := blackholeListener(t)
	stubMeshToolRunner(t, func(_ context.Context, name string, _ any, out any) error {
		switch name {
		case "outpost_mesh_link":
			meshToolResult(t, out, map[string]any{"link": admincore.MeshHostLinkView{
				Found: true, Direct: true, PeerID: "peer-target",
			}})
		case "outpost_mesh_resolve":
			meshToolResult(t, out, map[string]any{"peers": []admincore.MeshResolvedPeer{
				{Host: "target", PeerID: "peer-target"},
			}})
		case "outpost_mesh_listen":
			meshToolResult(t, out, map[string]any{"addr": fwdAddr})
		case "outpost_mesh_close_listen":
		default:
			t.Fatalf("unexpected mesh tool call %q", name)
		}
		return nil
	})
	fc := meshTestConfig(t, ticketStubServer(t).URL)

	start := time.Now()
	_, _, err := dialMeshDirect(context.Background(), fc, "bearer", "target", "user", "cookie")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("dialMeshDirect to blackhole succeeded, want error")
	}
	if errors.Is(err, errMeshNotAvailable) {
		t.Fatalf("dialMeshDirect error = %v, want an attempted-dial error", err)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("dialMeshDirect blackhole took %v, want fall-through inside the rung budget", elapsed)
	}
}

// And the tunnel rung: a relay that accepts but never answers must fail
// inside its budget too.
func TestDialCloudboxTunnelBlackholeFallsThroughWithinBudget(t *testing.T) {
	isolatedHome(t)
	shrinkRungTimeouts(t, time.Second)
	addr := blackholeListener(t)
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split blackhole addr: %v", err)
	}
	port, _ := strconv.Atoi(portStr)
	fc := &conf.FileConfig{ServerAddr: host, ServerPort: port, Protocol: "ws"}

	start := time.Now()
	_, _, err = dialCloudboxTunnel(context.Background(), fc, "bearer", "target", "user", "cookie")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("dialCloudboxTunnel to blackhole succeeded, want error")
	}
	if elapsed > 10*time.Second {
		t.Fatalf("dialCloudboxTunnel blackhole took %v, want fall-through inside the rung budget", elapsed)
	}
}

// The terminal error must name every rung and call out version skew
// instead of surfacing a bare timeout.
func TestSSHNoPathErrorMentionsRungsAndSkew(t *testing.T) {
	err := &sshNoPathError{
		Host:          "somehost",
		ClientVersion: "v1.0.0",
		PeerVersion:   "v0.32.0",
		LAN:           errLANNotAvailable,
		Mesh:          errMeshNotAvailable,
		Tunnel:        errors.New("dial: timeout"),
	}
	msg := err.Error()
	for _, want := range []string{"lan-direct", "mesh-direct", "tunnel", "v1.0.0", "v0.32.0", "version skew"} {
		if !strings.Contains(msg, want) {
			t.Errorf("terminal error %q missing %q", msg, want)
		}
	}

	same := &sshNoPathError{
		Host:          "somehost",
		ClientVersion: "v1.0.0",
		PeerVersion:   "v1.0.0",
		LAN:           errLANNotAvailable,
		Mesh:          errMeshNotAvailable,
		Tunnel:        errors.New("dial: timeout"),
	}
	if got := same.Error(); strings.Contains(got, "version skew") {
		t.Errorf("matching versions must not claim skew: %q", got)
	}

	unknown := &sshNoPathError{
		Host:   "somehost",
		LAN:    errLANNotAvailable,
		Mesh:   errMeshNotAvailable,
		Tunnel: errors.New("dial: timeout"),
	}
	for _, want := range []string{"lan-direct", "mesh-direct", "tunnel"} {
		if !strings.Contains(unknown.Error(), want) {
			t.Errorf("versionless error %q missing %q", unknown.Error(), want)
		}
	}
}

// lanAdvertisedVersion surfaces the peer's build for the skew check, and
// stays silent when the peer isn't visible.
func TestLANAdvertisedVersion(t *testing.T) {
	stubLANPeerLookup(t, &discovery.Peer{AgentName: "target", Version: "v1.0.0-rc.12"}, nil)
	if got := lanAdvertisedVersion(context.Background(), "target"); got != "v1.0.0-rc.12" {
		t.Fatalf("lanAdvertisedVersion = %q, want %q", got, "v1.0.0-rc.12")
	}
	stubLANPeerLookup(t, nil, errLANNotAvailable)
	if got := lanAdvertisedVersion(context.Background(), "target"); got != "" {
		t.Fatalf("lanAdvertisedVersion with no peer = %q, want empty", got)
	}
}
