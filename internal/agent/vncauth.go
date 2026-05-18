package agent

import (
	"context"
	"crypto/aes"
	"crypto/md5"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"math/big"
	"net"
	"strings"

	"github.com/coder/websocket"
)

// RFB security types we care about. macOS Screen Sharing always offers ARD
// (Apple Authentication, type 30); other types are rejected here. The
// browser path is gated separately on auth=None — see vncServeNoAuth.
const (
	rfbSecurityNone = 1
	rfbSecurityARD  = 30
)

// vncDialAuth opens a TCP connection to addr (a local VNC server, typically
// macOS Screen Sharing on 127.0.0.1:5900), completes the RFB ProtocolVersion
// exchange and Apple/ARD authentication using user+password, and returns
// the live conn plus the buffered ServerInit blob. After this returns the
// caller speaks RFB to the browser via vncServeNoAuth and then splices.
//
// Auth termination happens here (not in the browser) so the browser side
// doesn't need window.crypto.subtle — meaning Periscope works over plain
// HTTP / LAN-IP origins where the browser's isSecureContext is false.
func vncDialAuth(ctx context.Context, addr, user, password string) (net.Conn, []byte, error) {
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("dial vnc: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			conn.Close()
		}
	}()

	greet := make([]byte, 12)
	if _, err := io.ReadFull(conn, greet); err != nil {
		return nil, nil, fmt.Errorf("read server version: %w", err)
	}
	if !strings.HasPrefix(string(greet), "RFB ") {
		return nil, nil, fmt.Errorf("not an RFB server: %q", greet)
	}
	if _, err := conn.Write([]byte("RFB 003.008\n")); err != nil {
		return nil, nil, err
	}

	var nTypes [1]byte
	if _, err := io.ReadFull(conn, nTypes[:]); err != nil {
		return nil, nil, fmt.Errorf("read sectype count: %w", err)
	}
	if nTypes[0] == 0 {
		return nil, nil, readReasonError(conn, "security init failed")
	}
	types := make([]byte, nTypes[0])
	if _, err := io.ReadFull(conn, types); err != nil {
		return nil, nil, err
	}
	picked := byte(0)
	for _, t := range types {
		if t == rfbSecurityARD {
			picked = t
			break
		}
	}
	if picked == 0 {
		return nil, nil, fmt.Errorf("server does not offer Apple Authentication (got %v); enable Screen Sharing for macOS users", types)
	}
	if _, err := conn.Write([]byte{picked}); err != nil {
		return nil, nil, err
	}

	if err := doARDAuth(conn, user, password); err != nil {
		return nil, nil, err
	}

	var sr [4]byte
	if _, err := io.ReadFull(conn, sr[:]); err != nil {
		return nil, nil, fmt.Errorf("read security result: %w", err)
	}
	if binary.BigEndian.Uint32(sr[:]) != 0 {
		return nil, nil, readReasonError(conn, "auth rejected")
	}

	if _, err := conn.Write([]byte{1}); err != nil { // ClientInit: shared=1
		return nil, nil, err
	}

	head := make([]byte, 24)
	if _, err := io.ReadFull(conn, head); err != nil {
		return nil, nil, fmt.Errorf("read server init: %w", err)
	}
	nameLen := binary.BigEndian.Uint32(head[20:24])
	if nameLen > 1<<20 {
		return nil, nil, fmt.Errorf("server name absurdly long: %d", nameLen)
	}
	name := make([]byte, nameLen)
	if _, err := io.ReadFull(conn, name); err != nil {
		return nil, nil, err
	}
	init := append(head, name...)

	ok = true
	return conn, init, nil
}

// doARDAuth performs the Apple Remote Desktop / Screen Sharing Diffie-Hellman
// + AES-128-ECB credential exchange.
//
// Wire format on entry: 2-byte generator, 2-byte keyLength (bytes), then
// keyLength bytes of prime, then keyLength bytes of server public key. We
// reply with the 128-byte AES-encrypted credentials blob followed by our
// public key (keyLength bytes).
//
// AES key = MD5(sharedSecret); credentials block = 128 random bytes with
// username copied into [0,64) and password into [64,128), both NUL-terminated
// and truncated to 63 bytes. Matches noVNC's _negotiateARDAuthAsync.
func doARDAuth(conn net.Conn, user, password string) error {
	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return fmt.Errorf("read ard header: %w", err)
	}
	g := new(big.Int).SetBytes(head[:2])
	keyLen := int(binary.BigEndian.Uint16(head[2:4]))
	if keyLen < 64 || keyLen > 1024 {
		return fmt.Errorf("ard key length out of range: %d", keyLen)
	}
	params := make([]byte, 2*keyLen)
	if _, err := io.ReadFull(conn, params); err != nil {
		return fmt.Errorf("read ard params: %w", err)
	}
	p := new(big.Int).SetBytes(params[:keyLen])
	serverPub := new(big.Int).SetBytes(params[keyLen:])
	if p.Sign() <= 0 {
		return fmt.Errorf("ard: invalid prime")
	}

	priv, err := rand.Int(rand.Reader, p)
	if err != nil {
		return fmt.Errorf("ard rand: %w", err)
	}
	clientPub := new(big.Int).Exp(g, priv, p)
	shared := new(big.Int).Exp(serverPub, priv, p)
	clientPubBytes := leftPad(clientPub.Bytes(), keyLen)
	sharedBytes := leftPad(shared.Bytes(), keyLen)

	cred := make([]byte, 128)
	if _, err := rand.Read(cred); err != nil {
		return err
	}
	u, pw := []byte(user), []byte(password)
	if len(u) > 63 {
		u = u[:63]
	}
	if len(pw) > 63 {
		pw = pw[:63]
	}
	copy(cred[:64], u)
	cred[len(u)] = 0
	copy(cred[64:], pw)
	cred[64+len(pw)] = 0

	keyMD5 := md5.Sum(sharedBytes)
	block, err := aes.NewCipher(keyMD5[:])
	if err != nil {
		return err
	}
	enc := make([]byte, 128)
	for i := 0; i < 128; i += 16 {
		block.Encrypt(enc[i:i+16], cred[i:i+16])
	}

	out := make([]byte, 0, 128+keyLen)
	out = append(out, enc...)
	out = append(out, clientPubBytes...)
	if _, err := conn.Write(out); err != nil {
		return err
	}
	return nil
}

func leftPad(b []byte, n int) []byte {
	if len(b) >= n {
		return b
	}
	out := make([]byte, n)
	copy(out[n-len(b):], b)
	return out
}

func readReasonError(r io.Reader, prefix string) error {
	var l [4]byte
	if _, err := io.ReadFull(r, l[:]); err != nil {
		return fmt.Errorf("%s", prefix)
	}
	n := binary.BigEndian.Uint32(l[:])
	if n == 0 || n > 4096 {
		return fmt.Errorf("%s", prefix)
	}
	msg := make([]byte, n)
	if _, err := io.ReadFull(r, msg); err != nil {
		return fmt.Errorf("%s", prefix)
	}
	return fmt.Errorf("%s: %s", prefix, msg)
}

// wsReader turns a coder/websocket message stream into a byte reader so we
// can do io.ReadFull-style fixed-size reads during the RFB handshake.
type wsReader struct {
	ctx context.Context
	ws  *websocket.Conn
	buf []byte
}

func (r *wsReader) Read(p []byte) (int, error) {
	for len(r.buf) == 0 {
		_, data, err := r.ws.Read(r.ctx)
		if err != nil {
			return 0, err
		}
		r.buf = data
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}

// vncServeNoAuth speaks the server side of an RFB 3.8 handshake to the
// browser, advertising only security type None. After this returns the
// caller can byte-splice the post-init RFB stream.
//
// serverInit is the ServerInit blob captured during the agent's auth
// handshake with the actual VNC server; it is forwarded verbatim after
// the browser sends its ClientInit byte.
func vncServeNoAuth(ctx context.Context, ws *websocket.Conn, r *wsReader, serverInit []byte) error {
	if err := ws.Write(ctx, websocket.MessageBinary, []byte("RFB 003.008\n")); err != nil {
		return fmt.Errorf("send version: %w", err)
	}
	var clientVer [12]byte
	if _, err := io.ReadFull(r, clientVer[:]); err != nil {
		return fmt.Errorf("read client version: %w", err)
	}
	if err := ws.Write(ctx, websocket.MessageBinary, []byte{1, rfbSecurityNone}); err != nil {
		return fmt.Errorf("send sectypes: %w", err)
	}
	var pick [1]byte
	if _, err := io.ReadFull(r, pick[:]); err != nil {
		return fmt.Errorf("read sectype pick: %w", err)
	}
	if pick[0] != rfbSecurityNone {
		return fmt.Errorf("browser picked unexpected auth type: %d", pick[0])
	}
	if err := ws.Write(ctx, websocket.MessageBinary, []byte{0, 0, 0, 0}); err != nil {
		return fmt.Errorf("send security result: %w", err)
	}
	var shared [1]byte
	if _, err := io.ReadFull(r, shared[:]); err != nil {
		return fmt.Errorf("read client init: %w", err)
	}
	if err := ws.Write(ctx, websocket.MessageBinary, serverInit); err != nil {
		return fmt.Errorf("send server init: %w", err)
	}
	return nil
}
