package agent

import (
	"context"
	"fmt"
	"log/slog"

	v1 "github.com/fatedier/frp/pkg/config/v1"
	tunnelserver "github.com/fatedier/frp/server"
)

// TunnelServer runs the matrix-tunnel SERVER side (fatedier/frp's frps)
// in-process, so an outpost can host a control plane that other outposts
// reach exactly the way they reach cloudbox today.
//
// WHY THIS EXISTS. The DKS control plane is a placement choice, not three
// architectures (dhnt/docs/dks-control-plane-on-sphere.md): cloudbox, a
// rented always-on machine, or the user's own dev box. The node side is
// already identical in all three — `frpc` dials an frps and binds
// 127.0.0.1:<api-port> for `k3s agent`. The only piece that did not exist
// was an frps anywhere other than cloudbox. This is that piece.
//
// Written against the public frp API rather than ported from cloudbox's
// server. The two stay wire-compatible because they configure the same
// library at the same version (v0.68.1), not because they share code.
//
// WHAT THE CONTROL-PLANE HOST RUNS:
//
//	frps            (this file)               accepts frpc logins
//	frpc + STCP     (STCPProxy in tunnel.go)  publishes k3s-apiserver
//	k3s server      (runtime container)       the apiserver itself
//
// A worker's frpc then opens an STCP *visitor* onto that published proxy
// and binds it to its own loopback — which is why the agent needs no
// changes and no certificate work: it dials 127.0.0.1, already an
// apiserver SAN, in every placement.
type TunnelServer struct {
	svc *tunnelserver.Service
	log *slog.Logger
}

// TunnelServerConfig is the minimal server-side configuration.
type TunnelServerConfig struct {
	// BindAddr defaults to 127.0.0.1 — NOT 0.0.0.0. The expected reach
	// path is a mesh forward on this host, so binding loopback keeps the
	// tunnel off the LAN by default. Set it wider only deliberately.
	BindAddr string

	// BindPort defaults to 7000, matching the client's default.
	BindPort int

	// Token is the shared secret gating frpc logins. It authenticates the
	// SESSION, not any hostname — which is why a worker can dial this
	// server at whatever address reaches it without certificate
	// implications. Required: an empty token would accept any client.
	Token string
}

// NewTunnelServer builds (but does not start) the tunnel server.
func NewTunnelServer(tc TunnelServerConfig, log *slog.Logger) (*TunnelServer, error) {
	if tc.Token == "" {
		// Refuse rather than default. An frps with no token accepts every
		// client that can reach it, and the whole point of this server is
		// to front an apiserver.
		return nil, fmt.Errorf("tunnel server: Token is required (an empty token accepts any client)")
	}
	if log == nil {
		log = slog.Default()
	}

	cfg := &v1.ServerConfig{
		BindAddr: orDefault(tc.BindAddr, "127.0.0.1"),
		BindPort: orDefaultInt(tc.BindPort, 7000),
		Auth: v1.AuthServerConfig{
			Method: v1.AuthMethodToken,
			Token:  tc.Token,
		},
	}
	cfg.Complete()

	svc, err := tunnelserver.NewService(cfg)
	if err != nil {
		return nil, fmt.Errorf("tunnel server: %w", err)
	}
	return &TunnelServer{svc: svc, log: log}, nil
}

// Run serves tunnel clients. IT NEVER RETURNS — not on ctx cancellation, and
// not after Close(). Call it in a goroutine you do not wait on.
//
// This is measured upstream behaviour, not a guess. frp v0.68.1's
// Service.Run ends with a BLOCKING HandleListener on its muxed listener, and
// that loop exits only when the muxer's sub-listener yields an accept error —
// which neither cancelling the context nor Close() causes. A probe confirmed
// both: ctx cancel alone leaves Run blocked, and so does a subsequent Close.
//
// WHY THIS WARNING IS HERE IN CAPITALS. The obvious wiring — `g.Go(func()
// error { srv.Run(ctx); return nil })` — makes the errgroup's Wait() never
// return. Outpost self-restarts by cancelling that errgroup, waiting, and
// re-execing, so a tunnel server in the group would hang EVERY restart:
// pairing changes, builtin toggles, upgrades. The daemon would look alive
// while quietly refusing to apply anything. That failure is silent and
// remote-only, which is exactly the kind this comment has to prevent.
//
// The leak is survivable because Close DOES release the listeners (verified
// separately): the port frees for the next boot even though the accept
// goroutine stays parked. A leaked goroutine in a process that is about to
// re-exec costs nothing; a stuck port would cost the restart.
func (t *TunnelServer) Run(ctx context.Context) {
	t.log.Info("tunnel server: listening")
	t.svc.Run(ctx)
}

// Close stops the server and releases its listeners, freeing the bind port
// for the next boot. It does NOT unblock Run — see the note there.
func (t *TunnelServer) Close() error {
	if t.svc == nil {
		return nil
	}
	return t.svc.Close()
}
