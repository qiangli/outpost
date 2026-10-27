package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/outpost/internal/agent/admincore"
	"github.com/qiangli/outpost/internal/agent/mesh"
	"github.com/qiangli/outpost/internal/agent/storeroute"
)

// serveStoreRoute listens on 127.0.0.1:<storeroute.DefaultPort> and routes
// <peer>.<sat>.localhost to that peer's `s3` mesh service (self → the local
// s3 backend). Mesh links prefer the direct LAN path on their own.
func serveStoreRoute(ctx context.Context, self string, fwd *mesh.Forwarder, resolve func(string) ([]admincore.MeshResolvedPeer, error)) error {
	ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(storeroute.DefaultPort))
	if err != nil {
		slog.Warn("storeroute: listen failed", "err", err)
		return nil
	}
	var mu sync.Mutex
	fwds := map[string]string{} // peer id → local forward addr
	h := storeroute.Handler(func(_ context.Context, peer string) (string, error) {
		if strings.EqualFold(peer, self) {
			if a := fwd.Snapshot().Exposed["s3"]; a != "" {
				return a, nil
			}
			return "", fmt.Errorf("local s3 service is not exposed")
		}
		peers, err := resolve("s3")
		if err != nil {
			return "", err
		}
		for _, p := range peers {
			if p.PeerID == "" || !(strings.EqualFold(p.Host, peer) || p.PeerID == peer) {
				continue
			}
			mu.Lock()
			defer mu.Unlock()
			if a, ok := fwds[p.PeerID]; ok {
				return a, nil
			}
			l, err := fwd.Listen("", p.PeerID, "s3")
			if err != nil {
				return "", err
			}
			fwds[p.PeerID] = l.Addr().String()
			return fwds[p.PeerID], nil
		}
		return "", storeroute.ErrUnknownPeer
	})
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() { <-ctx.Done(); _ = srv.Close() }()
	_ = srv.Serve(ln)
	return nil
}
