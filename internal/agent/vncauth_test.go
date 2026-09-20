package agent

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// fakeRFB is an in-process VNC server that offers one security type and, for
// VNC Authentication, checks the DES response against a fixed challenge. It
// answers a minimal ServerInit so the relay's handshake can complete.
type fakeRFB struct {
	ln       net.Listener
	offer    []byte
	password string // expected for type 2
	name     string
}

func newFakeRFB(t *testing.T, offer []byte, password, name string) *fakeRFB {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeRFB{ln: ln, offer: offer, password: password, name: name}
	t.Cleanup(func() { _ = ln.Close() })
	go f.serve()
	return f
}

func (f *fakeRFB) serve() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.handle(c)
	}
}

func (f *fakeRFB) handle(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = c.Write([]byte("RFB 003.008\n"))
	ver := make([]byte, 12)
	if _, err := io.ReadFull(c, ver); err != nil {
		return
	}
	_, _ = c.Write(append([]byte{byte(len(f.offer))}, f.offer...))
	var pick [1]byte
	if _, err := io.ReadFull(c, pick[:]); err != nil {
		return
	}
	ok := true
	switch pick[0] {
	case rfbSecurityNone:
	case rfbSecurityVNC:
		var challenge [16]byte
		copy(challenge[:], "0123456789abcdef")
		_, _ = c.Write(challenge[:])
		var resp [16]byte
		if _, err := io.ReadFull(c, resp[:]); err != nil {
			return
		}
		ok = resp == vncAuthResponse(challenge, f.password)
	default:
		ok = false
	}
	if !ok {
		_, _ = c.Write([]byte{0, 0, 0, 1})
		reason := "authentication failed"
		var l [4]byte
		binary.BigEndian.PutUint32(l[:], uint32(len(reason)))
		_, _ = c.Write(append(l[:], reason...))
		return
	}
	_, _ = c.Write([]byte{0, 0, 0, 0})
	var shared [1]byte
	if _, err := io.ReadFull(c, shared[:]); err != nil {
		return
	}
	init := make([]byte, 24)
	binary.BigEndian.PutUint16(init[0:2], 800)
	binary.BigEndian.PutUint16(init[2:4], 600)
	binary.BigEndian.PutUint32(init[20:24], uint32(len(f.name)))
	_, _ = c.Write(append(init, f.name...))
	// Hold the connection open until the client goes away.
	_ = c.SetDeadline(time.Time{})
	_, _ = io.Copy(io.Discard, c)
}

func serverName(t *testing.T, init []byte) string {
	t.Helper()
	if len(init) < 24 {
		t.Fatalf("short ServerInit: %d bytes", len(init))
	}
	n := binary.BigEndian.Uint32(init[20:24])
	return string(init[24 : 24+n])
}

// The response for a known challenge/password pair, computed independently
// (RFC 6143 §7.2.2 with the bit-reversed key): pins the DES quirk.
func TestVNCAuthResponseVector(t *testing.T) {
	var challenge [16]byte
	copy(challenge[:], "0123456789abcdef")
	got := vncAuthResponse(challenge, "secret")
	// Reference from OpenSSL 3.x on the same inputs — the key is "secret"
	// zero-padded to 8 bytes with each byte's bits reversed (cea6c64ea62e0000):
	//   printf 0123456789abcdef | openssl enc -des-ecb -K cea6c64ea62e0000 \
	//     -nosalt -nopad -provider legacy -provider default | xxd -p
	const want = "752440ee2bfcc2a0d9013fd20371e23b"
	if hex.EncodeToString(got[:]) != want {
		t.Fatalf("vncAuthResponse = %x, want %s", got, want)
	}
}

func TestPickSecurityType(t *testing.T) {
	cases := []struct {
		offer []byte
		user  string
		want  byte
	}{
		{[]byte{30, 33, 36, 35}, "alice", 30}, // macOS, OS account → ARD
		{[]byte{30, 2}, "", 2},                // macOS with legacy VNC password, no user → VNC auth
		{[]byte{30, 2}, "alice", 30},          // both offered, user given → ARD
		{[]byte{2, 16}, "", 2},                // TightVNC
		{[]byte{2, 16}, "alice", 2},           // a user with no ARD on offer still gets VNC auth
		{[]byte{1}, "", 1},                    // None
		{[]byte{1, 2}, "", 2},                 // prefer a password over None when both are offered
		{[]byte{30}, "", 30},                  // ARD only, no user: try it and let the server refuse
		{[]byte{19}, "", 0},                   // VeNCrypt only: nothing we speak
	}
	for _, c := range cases {
		if got := pickSecurityType(c.offer, c.user); got != c.want {
			t.Errorf("pick(%v, %q) = %d, want %d", c.offer, c.user, got, c.want)
		}
	}
}

func TestVNCDialAuth_VNCAuthentication(t *testing.T) {
	srv := newFakeRFB(t, []byte{rfbSecurityVNC, 16}, "hunter2x", "win-desktop")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, init, err := vncDialAuth(ctx, srv.ln.Addr().String(), "", "hunter2x")
	if err != nil {
		t.Fatalf("type 2 accept: %v", err)
	}
	conn.Close()
	if got := serverName(t, init); got != "win-desktop" {
		t.Fatalf("server name = %q", got)
	}

	if _, _, err := vncDialAuth(ctx, srv.ln.Addr().String(), "", "wrong"); err == nil || !strings.Contains(err.Error(), "auth rejected") {
		t.Fatalf("type 2 reject: err = %v, want auth rejected", err)
	}
	if _, _, err := vncDialAuth(ctx, srv.ln.Addr().String(), "", ""); err == nil || !strings.Contains(err.Error(), "requires a password") {
		t.Fatalf("type 2 with no password: err = %v", err)
	}
}

func TestVNCDialAuth_None(t *testing.T) {
	srv := newFakeRFB(t, []byte{rfbSecurityNone}, "", "open-desktop")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, init, err := vncDialAuth(ctx, srv.ln.Addr().String(), "", "")
	if err != nil {
		t.Fatalf("none: %v", err)
	}
	conn.Close()
	if got := serverName(t, init); got != "open-desktop" {
		t.Fatalf("server name = %q", got)
	}
}

func TestVNCDialAuth_NothingUsable(t *testing.T) {
	srv := newFakeRFB(t, []byte{19}, "", "vencrypt-only")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := vncDialAuth(ctx, srv.ln.Addr().String(), "", "pw"); err == nil || !strings.Contains(err.Error(), "no security type") {
		t.Fatalf("err = %v", err)
	}
}
