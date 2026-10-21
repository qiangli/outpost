package agent

import (
	"context"
	"io"
	"net"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// expectEgressPolicyRefusal dials an egress app route and requires the
// server to terminate the WebSocket with a policy-violation close
// (the S487.3 refusal signal) instead of bridging any bytes.
func expectEgressPolicyRefusal(t *testing.T, wsURL string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer ws.Close(websocket.StatusNormalClosure, "done")
	ws.SetReadLimit(1024)
	if _, _, err := ws.Read(ctx); err == nil {
		t.Fatal("expected policy-violation close, read succeeded")
	} else if st := websocket.CloseStatus(err); st != websocket.StatusPolicyViolation {
		t.Fatalf("close status = %v, want %v (err: %v)", st, websocket.StatusPolicyViolation, err)
	}
}

// startTCPEcho starts a trivial TCP echo server on loopback for egress
// bridge tests. Returns the listener (caller closes) and its port.
func startTCPEcho(t *testing.T) (int, func()) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		for {
			c, aerr := l.Accept()
			if aerr != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}(c)
		}
	}()
	_, portStr, _ := net.SplitHostPort(l.Addr().String())
	port, _ := strconv.Atoi(portStr)
	return port, func() { _ = l.Close() }
}

// TestEgressAppDialsAllowlistedAndRefusesOthers: an egress app bridges
// a WebSocket to dst=host:port only when the destination matches its
// allowlist AND survives the forbidden-IP screen. A non-listed host is
// refused, and the metadata IP is refused even though it is explicitly
// allowlisted (loopback/link-local/unspecified always lose).
func TestEgressAppDialsAllowlistedAndRefusesOthers(t *testing.T) {
	egressAllowLoopbackForTest = true
	t.Cleanup(func() { egressAllowLoopbackForTest = false })

	port, done := startTCPEcho(t)
	defer done()

	reg := NewAppRegistry()
	allow := []string{
		"127.0.0.1:" + strconv.Itoa(port),
		"169.254.169.254:80",
	}
	if err := reg.RegisterFromConfig(conf.AppConfig{
		Name: "eg", Scheme: "egress", Allow: allow, Enabled: true,
	}); err != nil {
		t.Fatalf("register egress: %v", err)
	}

	srv := httptest.NewServer(newTestRouter(reg))
	defer srv.Close()
	base := strings.Replace(srv.URL, "http", "ws", 1) + "/app/eg/"

	// Allowed destination: echo round-trips through the bridge.
	func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ws, _, err := websocket.Dial(ctx, base+"?dst=127.0.0.1:"+strconv.Itoa(port), nil)
		if err != nil {
			t.Fatalf("ws dial allowed dst: %v", err)
		}
		defer ws.Close(websocket.StatusNormalClosure, "done")
		conn := websocket.NetConn(ctx, ws, websocket.MessageBinary)
		defer conn.Close()
		want := []byte("hello egress")
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
	}()

	// Non-listed host: refused without dialing.
	expectEgressPolicyRefusal(t, base+"?dst=blocked.example:443")

	// Metadata IP: refused even though explicitly allowlisted.
	expectEgressPolicyRefusal(t, base+"?dst=169.254.169.254:80")
}

// TestEgressEmptyAllowlistRefusesAll: an egress app with no allow
// entries is default-deny — every destination is refused.
func TestEgressEmptyAllowlistRefusesAll(t *testing.T) {
	egressAllowLoopbackForTest = true
	t.Cleanup(func() { egressAllowLoopbackForTest = false })

	port, done := startTCPEcho(t)
	defer done()

	reg := NewAppRegistry()
	if err := reg.RegisterFromConfig(conf.AppConfig{
		Name: "eg", Scheme: "egress", Enabled: true,
	}); err != nil {
		t.Fatalf("register egress: %v", err)
	}

	srv := httptest.NewServer(newTestRouter(reg))
	defer srv.Close()
	base := strings.Replace(srv.URL, "http", "ws", 1) + "/app/eg/"

	expectEgressPolicyRefusal(t, base+"?dst=127.0.0.1:"+strconv.Itoa(port))
}
