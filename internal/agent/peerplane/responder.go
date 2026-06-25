package peerplane

import (
	"context"
	"net"
)

// EchoResponder is a UDP server that replies "pong" to any "ping" datagram, so
// peers can measure RTT to this host on whatever interface they can reach. It
// binds 0.0.0.0:port (port 0 ⇒ ephemeral); Port() returns the chosen port,
// which is what the host announces in its candidates.
type EchoResponder struct {
	conn *net.UDPConn
}

// NewEchoResponder binds the responder. port 0 picks an ephemeral port.
func NewEchoResponder(port int) (*EchoResponder, error) {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero, Port: port})
	if err != nil {
		return nil, err
	}
	return &EchoResponder{conn: conn}, nil
}

// Port is the bound UDP port — announce candidates with this port.
func (r *EchoResponder) Port() int {
	return r.conn.LocalAddr().(*net.UDPAddr).Port
}

// Run serves until ctx is cancelled (or the socket closes).
func (r *EchoResponder) Run(ctx context.Context) {
	go func() { <-ctx.Done(); _ = r.conn.Close() }()
	buf := make([]byte, 64)
	for {
		c, src, err := r.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if c == 4 && string(buf[:c]) == "ping" {
			_, _ = r.conn.WriteToUDP([]byte("pong"), src)
		}
	}
}

// Close stops the responder.
func (r *EchoResponder) Close() error { return r.conn.Close() }
