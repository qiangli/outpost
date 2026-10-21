package agent

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// bindNonLoopbackListener searches for a non-127.0.0.1 local interface address
// that can accept a TCP listener. If one is found, it returns the listener, host,
// and port. If none exists (e.g. in a loopback-only environment), it skips the test.
func bindNonLoopbackListener(t *testing.T) (net.Listener, string, int) {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Skipf("cannot query local network interfaces: %v", err)
	}

	// First pass: try non-loopback IPv4 addresses.
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
				continue
			}
			ip4 := ip.To4()
			if ip4 == nil || ip4[0] == 127 {
				continue
			}
			l, err := net.Listen("tcp", net.JoinHostPort(ip4.String(), "0"))
			if err == nil {
				host, portStr, err := net.SplitHostPort(l.Addr().String())
				if err != nil {
					_ = l.Close()
					continue
				}
				port, err := strconv.Atoi(portStr)
				if err != nil {
					_ = l.Close()
					continue
				}
				return l, host, port
			}
		}
	}

	// Second pass: try non-loopback IPv6 addresses if no IPv4 succeeded.
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
				continue
			}
			if ip.To4() != nil {
				continue
			}
			l, err := net.Listen("tcp", net.JoinHostPort(ip.String(), "0"))
			if err == nil {
				host, portStr, err := net.SplitHostPort(l.Addr().String())
				if err != nil {
					_ = l.Close()
					continue
				}
				port, err := strconv.Atoi(portStr)
				if err != nil {
					_ = l.Close()
					continue
				}
				return l, host, port
			}
		}
	}

	t.Skip("no non-127.0.0.1 local interface address available to bind")
	return nil, "", 0
}

// TestTCPAppForwardsToNonLoopbackTarget proves that a tcp-scheme app registered
// with a non-loopback host forwards traffic to that target over WebSocket.
func TestTCPAppForwardsToNonLoopbackTarget(t *testing.T) {
	ln, host, port := bindNonLoopbackListener(t)
	defer ln.Close()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()

	reg := NewAppRegistry()
	if err := reg.RegisterFromConfig(conf.AppConfig{
		Name:    "db",
		Scheme:  "tcp",
		Host:    host,
		Port:    port,
		Enabled: true,
	}); err != nil {
		t.Fatalf("register tcp app: %v", err)
	}

	srv := httptest.NewServer(newTestRouter(reg))
	defer srv.Close()

	wsURL := strings.Replace(srv.URL, "http", "ws", 1) + "/app/db/"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ws, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer ws.Close(websocket.StatusNormalClosure, "done")

	conn := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	defer conn.Close()

	want := []byte("hello bastion echo")
	if _, err := conn.Write(want); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := make([]byte, len(want))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("echo got %q, want %q", got, want)
	}
}

// TestTCPAppIgnoresClientSuppliedDestination proves that a tcp-scheme app is a
// fixed-destination forward: any client attempt to redirect the upstream connection
// via query parameters (?dst=...) or headers (X-Forwarded-Host, Host) is ignored,
// traffic strictly reaches the configured target listener A, and the forged
// destination listener B never accepts a connection.
func TestTCPAppIgnoresClientSuppliedDestination(t *testing.T) {
	// Listener A: the registered target (echo server).
	lnA, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen A: %v", err)
	}
	defer lnA.Close()

	go func() {
		for {
			conn, err := lnA.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()

	hostA, portStrA, err := net.SplitHostPort(lnA.Addr().String())
	if err != nil {
		t.Fatalf("split host port A: %v", err)
	}
	portA, err := strconv.Atoi(portStrA)
	if err != nil {
		t.Fatalf("parse port A: %v", err)
	}

	// Listener B: the attacker-supplied destination that MUST NOT be dialed.
	var bAccepted atomic.Int32
	lnB, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen B: %v", err)
	}
	defer lnB.Close()

	go func() {
		for {
			conn, err := lnB.Accept()
			if err != nil {
				return
			}
			bAccepted.Add(1)
			_ = conn.Close()
		}
	}()

	addrB := lnB.Addr().String()

	reg := NewAppRegistry()
	if err := reg.RegisterFromConfig(conf.AppConfig{
		Name:    "db",
		Scheme:  "tcp",
		Host:    hostA,
		Port:    portA,
		Enabled: true,
	}); err != nil {
		t.Fatalf("register tcp app: %v", err)
	}

	srv := httptest.NewServer(newTestRouter(reg))
	defer srv.Close()

	baseWS := strings.Replace(srv.URL, "http", "ws", 1) + "/app/db/"

	t.Run("dst query parameter ignored", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		ws, _, err := websocket.Dial(ctx, baseWS+"?dst="+addrB, nil)
		if err != nil {
			t.Fatalf("ws dial with dst query: %v", err)
		}
		defer ws.Close(websocket.StatusNormalClosure, "done")

		conn := websocket.NetConn(ctx, ws, websocket.MessageBinary)
		defer conn.Close()

		want := []byte("ping to listener A via query dst")
		if _, err := conn.Write(want); err != nil {
			t.Fatalf("write: %v", err)
		}
		got := make([]byte, len(want))
		if _, err := io.ReadFull(conn, got); err != nil {
			t.Fatalf("read: %v", err)
		}
		if string(got) != string(want) {
			t.Fatalf("got %q, want %q", got, want)
		}
		if count := bAccepted.Load(); count != 0 {
			t.Fatalf("listener B accepted %d connections, want 0", count)
		}
	})

	t.Run("X-Forwarded-Host header ignored", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		opts := &websocket.DialOptions{
			HTTPHeader: http.Header{
				"X-Forwarded-Host": []string{addrB},
			},
		}
		ws, _, err := websocket.Dial(ctx, baseWS, opts)
		if err != nil {
			t.Fatalf("ws dial with X-Forwarded-Host: %v", err)
		}
		defer ws.Close(websocket.StatusNormalClosure, "done")

		conn := websocket.NetConn(ctx, ws, websocket.MessageBinary)
		defer conn.Close()

		want := []byte("ping to listener A via X-Forwarded-Host")
		if _, err := conn.Write(want); err != nil {
			t.Fatalf("write: %v", err)
		}
		got := make([]byte, len(want))
		if _, err := io.ReadFull(conn, got); err != nil {
			t.Fatalf("read: %v", err)
		}
		if string(got) != string(want) {
			t.Fatalf("got %q, want %q", got, want)
		}
		if count := bAccepted.Load(); count != 0 {
			t.Fatalf("listener B accepted %d connections, want 0", count)
		}
	})

	t.Run("Host header naming B ignored", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		opts := &websocket.DialOptions{
			Host: addrB,
		}
		ws, _, err := websocket.Dial(ctx, baseWS, opts)
		if err != nil {
			t.Fatalf("ws dial with Host: %v", err)
		}
		defer ws.Close(websocket.StatusNormalClosure, "done")

		conn := websocket.NetConn(ctx, ws, websocket.MessageBinary)
		defer conn.Close()

		want := []byte("ping to listener A via Host")
		if _, err := conn.Write(want); err != nil {
			t.Fatalf("write: %v", err)
		}
		got := make([]byte, len(want))
		if _, err := io.ReadFull(conn, got); err != nil {
			t.Fatalf("read: %v", err)
		}
		if string(got) != string(want) {
			t.Fatalf("got %q, want %q", got, want)
		}
		if count := bAccepted.Load(); count != 0 {
			t.Fatalf("listener B accepted %d connections, want 0", count)
		}
	})

	t.Run("combined query and headers naming B ignored", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		opts := &websocket.DialOptions{
			Host: addrB,
			HTTPHeader: http.Header{
				"X-Forwarded-Host": []string{addrB},
			},
		}
		ws, _, err := websocket.Dial(ctx, baseWS+"?dst="+addrB, opts)
		if err != nil {
			t.Fatalf("ws dial with combined attack: %v", err)
		}
		defer ws.Close(websocket.StatusNormalClosure, "done")

		conn := websocket.NetConn(ctx, ws, websocket.MessageBinary)
		defer conn.Close()

		want := []byte("ping to listener A via combined attack")
		if _, err := conn.Write(want); err != nil {
			t.Fatalf("write: %v", err)
		}
		got := make([]byte, len(want))
		if _, err := io.ReadFull(conn, got); err != nil {
			t.Fatalf("read: %v", err)
		}
		if string(got) != string(want) {
			t.Fatalf("got %q, want %q", got, want)
		}
		if count := bAccepted.Load(); count != 0 {
			t.Fatalf("listener B accepted %d connections, want 0", count)
		}
	})

	if count := bAccepted.Load(); count != 0 {
		t.Fatalf("listener B accepted %d connections in total, want 0", count)
	}
}
