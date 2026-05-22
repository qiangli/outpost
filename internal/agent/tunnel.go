package agent

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	tunnelclient "github.com/fatedier/frp/client"
	tunnelconfig "github.com/fatedier/frp/pkg/config"
	"github.com/fatedier/frp/pkg/config/source"
	v1 "github.com/fatedier/frp/pkg/config/v1"
	"github.com/samber/lo"
)

// TunnelConfig is the minimal config for embedding the matrix-tunnel
// client (the underlying transport is fatedier/frp; we keep the import
// for the implementation but consistently refer to it as the matrix
// tunnel everywhere else).
type TunnelConfig struct {
	ServerAddr string // required, e.g. "cloud.example.com"
	ServerPort int    // default 7000
	Token      string // shared secret with the cloudbox matrix-tunnel server
	User       string // optional proxy-name prefix; sets ClientCommonConfig.User

	// Protocol is the matrix-tunnel transport: "tcp" (default), "ws",
	// or "wss". When ws/wss is selected the agent dials cloudbox's HTTPS
	// port at the well-known path /~!frp (hardcoded by the underlying
	// tunnel library) and cloudbox's WS bridge pipes the upgraded conn
	// to the loopback tunnel server. Cloudflare/DO App Platform only
	// route HTTP(S) — wss is what makes the prod tunnel work.
	Protocol string
}

// TCPProxy declares one local TCP service that should be reachable from
// the matrix-tunnel server's loopback. RemotePort=0 lets the server
// auto-assign; we usually pin it.
type TCPProxy struct {
	Name       string
	LocalIP    string
	LocalPort  int
	RemotePort int
}

// Reconnect tuning for the supervisor loop in Run. Exported as vars (not
// consts) so tests can shrink them; production paths shouldn't touch.
var (
	reconnectInitialBackoff = 2 * time.Second
	reconnectMaxBackoff     = 30 * time.Second
)

// Tunnel wraps a matrix-tunnel client. Call Run, then Close.
//
// The embedded FRP Service is one-shot — once its Run returns, its
// internal goroutines are torn down and it cannot be restarted. The
// library has its own reconnect loop, but we've observed it give up
// silently on yamux "session shutdown" (FRP client/control.go:130),
// leaving the outpost process alive with no tunnel. Tunnel.Run wraps
// svc.Run in a supervisor that rebuilds the service whenever it exits
// before ctx is canceled.
type Tunnel struct {
	cfg     TunnelConfig
	proxies []TCPProxy

	mu  sync.Mutex
	svc *tunnelclient.Service
}

// NewTunnel builds the matrix-tunnel client with the given proxies
// pre-registered via the in-memory ConfigSource — no config-file path
// involved.
func NewTunnel(tc TunnelConfig, proxies []TCPProxy) (*Tunnel, error) {
	svc, err := newTunnelService(tc, proxies)
	if err != nil {
		return nil, err
	}
	return &Tunnel{cfg: tc, proxies: proxies, svc: svc}, nil
}

// newTunnelService builds a fresh FRP Service from the same config — used
// both at first New and by the Run supervisor when the previous Service
// exited.
func newTunnelService(tc TunnelConfig, proxies []TCPProxy) (*tunnelclient.Service, error) {
	// LoginFailExit defaults to true, which makes the agent exit if the
	// matrix-tunnel server isn't reachable on the first dial. For a
	// long-running home-host agent that needs to survive cloud restarts
	// (and `make start` race conditions), false + the tunnel library's
	// built-in retry is what we want.
	loginFailExit := false

	common := &v1.ClientCommonConfig{
		ServerAddr:    tc.ServerAddr,
		ServerPort:    orDefaultInt(tc.ServerPort, 7000),
		User:          tc.User,
		LoginFailExit: &loginFailExit,
		Auth: v1.AuthClientConfig{
			Method: v1.AuthMethodToken,
			Token:  tc.Token,
		},
	}
	switch tc.Protocol {
	case "websocket", "wss":
		// Disable the tunnel's app-layer TLS — Cloudflare / DO App
		// Platform already terminates TLS at the edge, and double-
		// wrapping breaks the wss handshake. HeartbeatInterval=30 is
		// mandatory: Cloudflare reaps idle WebSockets at ~100 s, App
		// Platform at ~60 s, and the tunnel library's default heartbeat
		// is disabled (-1) which kills the control conn.
		common.Transport.Protocol = tc.Protocol
		common.Transport.TLS.Enable = lo.ToPtr(false)
		common.Transport.HeartbeatInterval = 30
	case "", "tcp":
		// Default raw-TCP transport; nothing to set.
	default:
		return nil, fmt.Errorf("unsupported matrix-tunnel protocol %q (expected tcp/websocket/wss)", tc.Protocol)
	}
	if err := common.Complete(); err != nil {
		return nil, fmt.Errorf("matrix-tunnel client common: %w", err)
	}

	configurers := make([]v1.ProxyConfigurer, 0, len(proxies))
	for _, p := range proxies {
		pc := &v1.TCPProxyConfig{
			ProxyBaseConfig: v1.ProxyBaseConfig{
				Name: p.Name,
				Type: "tcp",
				ProxyBackend: v1.ProxyBackend{
					LocalIP:   orDefault(p.LocalIP, "127.0.0.1"),
					LocalPort: p.LocalPort,
				},
			},
			RemotePort: p.RemotePort,
		}
		configurers = append(configurers, pc)
	}
	configurers = tunnelconfig.CompleteProxyConfigurers(configurers)

	cs := source.NewConfigSource()
	if err := cs.ReplaceAll(configurers, nil); err != nil {
		return nil, fmt.Errorf("matrix-tunnel client proxies: %w", err)
	}
	agg := source.NewAggregator(cs)

	svc, err := tunnelclient.NewService(tunnelclient.ServiceOptions{
		Common:                 common,
		ConfigSourceAggregator: agg,
	})
	if err != nil {
		return nil, fmt.Errorf("matrix-tunnel client new: %w", err)
	}
	return svc, nil
}

// Run blocks until ctx is canceled. If the underlying FRP service exits
// early (e.g. yamux session shutdown that its own retry loop swallows),
// rebuild it and try again with exponential backoff. Only ctx
// cancellation terminates the loop.
func (t *Tunnel) Run(ctx context.Context) error {
	backoff := reconnectInitialBackoff
	for {
		t.mu.Lock()
		svc := t.svc
		t.mu.Unlock()

		runErr := svc.Run(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if runErr != nil {
			slog.Warn("matrix-tunnel exited; reconnecting", "err", runErr, "backoff", backoff)
		} else {
			slog.Warn("matrix-tunnel exited unexpectedly; reconnecting", "backoff", backoff)
		}

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}

		next, err := newTunnelService(t.cfg, t.proxies)
		if err != nil {
			// A rebuild failure here means our config is fundamentally
			// rejected (bad proxy spec, etc.) — there's no point thrashing.
			// Back off to the cap and keep trying; an operator fix can
			// land via a restart.
			slog.Error("matrix-tunnel rebuild failed", "err", err)
			backoff = growBackoff(backoff)
			continue
		}
		t.mu.Lock()
		t.svc = next
		t.mu.Unlock()
		backoff = growBackoff(backoff)
	}
}

func growBackoff(cur time.Duration) time.Duration {
	next := cur * 2
	if next > reconnectMaxBackoff {
		return reconnectMaxBackoff
	}
	return next
}

// Close releases client resources.
func (t *Tunnel) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.svc.Close()
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func orDefaultInt(n, def int) int {
	if n == 0 {
		return def
	}
	return n
}
