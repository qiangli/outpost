package agent

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// The mesh-published handler serves the relay and nothing else — a peer must
// never find /shell or /clipboard behind the "desktop" service.
func TestMeshDesktopHandlerServesOnlyDesktop(t *testing.T) {
	srv := httptest.NewServer(MeshDesktopHandler("127.0.0.1:1"))
	defer srv.Close()
	for _, p := range []string{"/shell", "/clipboard", "/apps", "/auth", "/"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", p, resp.StatusCode)
		}
	}
}

// readFull drains RFB bytes from the browser side of the websocket.
func wsReadFull(ctx context.Context, t *testing.T, ws *websocket.Conn, buf *[]byte, n int) []byte {
	t.Helper()
	for len(*buf) < n {
		_, data, err := ws.Read(ctx)
		if err != nil {
			t.Fatalf("ws read: %v", err)
		}
		*buf = append(*buf, data...)
	}
	out := (*buf)[:n]
	*buf = (*buf)[n:]
	return out
}

// End to end through the relay: the browser sends the credentials frame, the
// relay authenticates against a VNC-password (type 2) server on its behalf,
// and the browser completes an auth-None RFB handshake and receives the real
// ServerInit — the wire a Windows/Linux leg rides.
func TestDesktopRelayVNCPasswordRoundTrip(t *testing.T) {
	vnc := newFakeRFB(t, []byte{rfbSecurityVNC}, "hunter2x", "linux-desktop")
	srv := httptest.NewServer(MeshDesktopHandler(vnc.ln.Addr().String()))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/desktop", &websocket.DialOptions{Subprotocols: []string{"binary"}})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close(websocket.StatusNormalClosure, "")

	creds, _ := json.Marshal(vncCreds{User: "", Password: "hunter2x"})
	if err := ws.Write(ctx, websocket.MessageText, creds); err != nil {
		t.Fatal(err)
	}
	var buf []byte
	if got := string(wsReadFull(ctx, t, ws, &buf, 12)); got != "RFB 003.008\n" {
		t.Fatalf("server version = %q", got)
	}
	_ = ws.Write(ctx, websocket.MessageBinary, []byte("RFB 003.008\n"))
	if got := wsReadFull(ctx, t, ws, &buf, 2); got[0] != 1 || got[1] != rfbSecurityNone {
		t.Fatalf("browser offered %v, want [1 None]", got)
	}
	_ = ws.Write(ctx, websocket.MessageBinary, []byte{rfbSecurityNone})
	if got := wsReadFull(ctx, t, ws, &buf, 4); binary.BigEndian.Uint32(got) != 0 {
		t.Fatalf("security result = %v", got)
	}
	_ = ws.Write(ctx, websocket.MessageBinary, []byte{1}) // ClientInit shared
	head := wsReadFull(ctx, t, ws, &buf, 24)
	n := int(binary.BigEndian.Uint32(head[20:24]))
	name := wsReadFull(ctx, t, ws, &buf, n)
	if string(name) != "linux-desktop" {
		t.Fatalf("ServerInit name = %q", name)
	}
}

// A wrong VNC password closes the websocket before any RFB byte reaches the
// browser, with the server's reason.
func TestDesktopRelayRejectsBadPassword(t *testing.T) {
	vnc := newFakeRFB(t, []byte{rfbSecurityVNC}, "hunter2x", "linux-desktop")
	srv := httptest.NewServer(MeshDesktopHandler(vnc.ln.Addr().String()))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/desktop", &websocket.DialOptions{Subprotocols: []string{"binary"}})
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close(websocket.StatusNormalClosure, "")
	creds, _ := json.Marshal(vncCreds{Password: "nope"})
	_ = ws.Write(ctx, websocket.MessageText, creds)
	_, _, err = ws.Read(ctx)
	if err == nil {
		t.Fatal("expected the relay to close the socket")
	}
	if st := websocket.CloseStatus(err); st != websocket.StatusPolicyViolation {
		t.Fatalf("close status = %v (%v), want policy violation", st, err)
	}
	if !strings.Contains(err.Error(), "auth rejected") {
		t.Fatalf("close reason = %v, want auth rejected", err)
	}
	_ = io.EOF
}
