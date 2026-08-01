package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// freePort returns a port nothing is listening on. Racy in principle,
// fine in practice, and far better than a hardcoded port that collides
// with whatever else the machine is running.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func waitDial(t *testing.T, addr string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
		if err == nil {
			c.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("nothing listening on %s after %s", addr, d)
}

// TestTunnelServer_STCPRoundTrip is the whole control-plane-on-a-peer
// mechanism in one test, with no cloudbox and no network beyond loopback:
//
//	origin      an HTTP server standing in for the apiserver
//	frps        THIS package's TunnelServer — the piece that did not exist
//	publisher   frpc + STCPProxy, offering the origin by name + secret
//	consumer    frpc + STCPVisitor, binding it back onto its own loopback
//
// A worker's k3s agent is the consumer's client. It dials 127.0.0.1 and
// is unaware of any of this — which is the property that makes the
// control plane's location a configuration choice.
func TestTunnelServer_STCPRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("spins up real listeners; skipped under -short")
	}

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "apiserver-stand-in")
	}))
	defer origin.Close()
	originPort := origin.Listener.Addr().(*net.TCPAddr).Port

	const token, secret = "tunnel-token", "stcp-secret"
	srvPort := freePort(t)

	srv, err := NewTunnelServer(TunnelServerConfig{
		BindAddr: "127.0.0.1", BindPort: srvPort, Token: token,
	}, slog.Default())
	if err != nil {
		t.Fatalf("NewTunnelServer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Run(ctx)
	defer srv.Close()
	waitDial(t, fmt.Sprintf("127.0.0.1:%d", srvPort), 10*time.Second)

	common := TunnelConfig{ServerAddr: "127.0.0.1", ServerPort: srvPort, Token: token, Protocol: "tcp"}

	// Publisher: the control-plane host offering its apiserver.
	pubCfg := common
	pubCfg.User = "controlplane"
	pub, err := NewTunnel(pubCfg, nil, nil, []STCPProxy{{
		Name: "k3s-apiserver", LocalIP: "127.0.0.1", LocalPort: originPort, Secret: secret,
		AllowUsers: []string{"worker"},
	}})
	if err != nil {
		t.Fatalf("publisher: %v", err)
	}
	go pub.Run(ctx)
	defer pub.Close()

	// Consumer: the worker, binding the published service to ITS loopback.
	visitorPort := freePort(t)
	conCfg := common
	conCfg.User = "worker"
	con, err := NewTunnel(conCfg, nil, []STCPVisitor{{
		Name:       "k3s-apiserver-visitor",
		ServerUser: "controlplane",
		ServerName: "k3s-apiserver",
		Secret:     secret,
		BindAddr:   "127.0.0.1",
		BindPort:   visitorPort,
	}}, nil)
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}
	go con.Run(ctx)
	defer con.Close()

	waitDial(t, fmt.Sprintf("127.0.0.1:%d", visitorPort), 20*time.Second)

	// The assertion that matters: bytes reach the origin through
	// visitor → frps → publisher, and come back intact.
	var body string
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", visitorPort))
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			body = string(b)
			if body != "" {
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	if body != "apiserver-stand-in" {
		t.Fatalf("round trip through the tunnel returned %q, want %q", body, "apiserver-stand-in")
	}
}

// An frps with no token accepts every client that can reach it, and this
// server exists to front an apiserver. Refusing beats defaulting.
func TestNewTunnelServer_RequiresToken(t *testing.T) {
	if _, err := NewTunnelServer(TunnelServerConfig{BindPort: 7000}, nil); err == nil {
		t.Fatal("expected an error when Token is empty")
	}
}
