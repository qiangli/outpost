package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/qiangli/outpost/internal/agent/admincore"
	"github.com/qiangli/outpost/internal/agent/conf"
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
