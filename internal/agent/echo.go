package agent

import (
	"io"
	"net"
)

// StartEchoServer starts a trivial TCP echo listener on addr (usually
// "127.0.0.1:0" so the kernel picks a port). Used by the matrix-tunnel
// round-trip integration test; not wired into the production agent.
func StartEchoServer(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
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
	return ln, nil
}
