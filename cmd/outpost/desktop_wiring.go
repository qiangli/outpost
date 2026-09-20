package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/qiangli/outpost/internal/agent"
)

// MeshServiceDesktop is the mesh service name under which an outpost publishes
// its /desktop VNC relay to authenticated mesh peers. A peer's console reaches
// it with `outpost mesh listen <peer-id> desktop` and speaks the same wire the
// cloudbox path does (a JSON credentials frame, then RFB) — no cloudbox on the
// path. Published whenever Desktop is on and the mesh host is up; nothing to
// configure, the same rule as MeshServiceSSH.
const MeshServiceDesktop = "desktop"

// startMeshDesktopListener binds a loopback-only listener serving ONLY the
// desktop relay and exposes it over the mesh. Never the whole local engine:
// see agent.MeshDesktopHandler for why.
func startMeshDesktopListener(
	gctx context.Context,
	g *errgroup.Group,
	vncAddr string,
	expose func(service, addr string),
	alreadyExposed func(service string) bool,
) string {
	if expose == nil {
		return ""
	}
	if alreadyExposed != nil && alreadyExposed(MeshServiceDesktop) {
		slog.Info("mesh desktop: service name already exposed by config — leaving it alone",
			"service", MeshServiceDesktop)
		return ""
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		slog.Warn("mesh desktop: listen failed", "err", err)
		return ""
	}
	bound := ln.Addr().String()
	srv := &http.Server{
		Handler:           agent.MeshDesktopHandler(vncAddr),
		ReadHeaderTimeout: 10 * time.Second,
	}
	g.Go(func() error {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Warn("mesh desktop: serve exited", "err", err)
		}
		return nil
	})
	g.Go(func() error {
		<-gctx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shCtx)
		return nil
	})
	expose(MeshServiceDesktop, bound)
	slog.Info("mesh desktop: published to mesh peers", "service", MeshServiceDesktop, "addr", bound)
	return bound
}
