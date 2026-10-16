package runtimecmd

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/spf13/cobra"
)

func tlsproxyCmd() *cobra.Command {
	var listen, forward, cert, key string
	cmd := &cobra.Command{Use: "tlsproxy", Short: "Forward TCP, optionally terminating incoming TLS", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signalContext(cmd)
			defer cancel()
			ln, err := proxyListener(listen, cert, key)
			if err != nil {
				return err
			}
			return serveProxy(ctx, ln, forward)
		}}
	cmd.Flags().StringVar(&listen, "listen", "", "Listen host:port")
	cmd.Flags().StringVar(&forward, "forward", "", "Forward TCP host:port")
	cmd.Flags().StringVar(&cert, "cert", "", "TLS certificate PEM (omit for TCP passthrough)")
	cmd.Flags().StringVar(&key, "key", "", "TLS private key PEM")
	_ = cmd.MarkFlagRequired("listen")
	_ = cmd.MarkFlagRequired("forward")
	return cmd
}

func proxyListener(addr, cert, key string) (net.Listener, error) {
	if (cert == "") != (key == "") {
		return nil, fmt.Errorf("cert and key must be provided together")
	}
	var cfg *tls.Config
	if cert != "" {
		pair, err := tls.LoadX509KeyPair(cert, key)
		if err != nil {
			return nil, err
		}
		cfg = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	if cfg != nil {
		ln = tls.NewListener(ln, cfg)
	}
	return ln, nil
}

func serveProxy(ctx context.Context, ln net.Listener, forward string) error {
	defer ln.Close()
	if _, _, err := net.SplitHostPort(forward); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn, err := ln.Accept()
		if err != nil {
			canceled := ctx.Err() != nil
			cancel()
			if canceled {
				return nil
			}
			return err
		}
		wg.Add(1)
		go func() { defer wg.Done(); proxyConnection(ctx, conn, forward) }()
	}
}

func proxyConnection(ctx context.Context, downstream net.Conn, forward string) {
	defer downstream.Close()
	stop := context.AfterFunc(ctx, func() { _ = downstream.Close() })
	defer stop()
	upstream, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", forward)
	if err != nil {
		return
	}
	defer upstream.Close()
	stopUp := context.AfterFunc(ctx, func() { _ = upstream.Close() })
	defer stopUp()
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(upstream, downstream)
		if cw, ok := upstream.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		close(done)
	}()
	_, _ = io.Copy(downstream, upstream)
	_ = downstream.Close()
	_ = upstream.Close()
	<-done
}
