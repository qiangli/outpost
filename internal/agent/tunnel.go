package agent

import (
	"context"
	"fmt"

	frpclient "github.com/fatedier/frp/client"
	frpconfig "github.com/fatedier/frp/pkg/config"
	"github.com/fatedier/frp/pkg/config/source"
	v1 "github.com/fatedier/frp/pkg/config/v1"
	"github.com/samber/lo"
)

// TunnelConfig is the minimal config for embedding frpc.
type TunnelConfig struct {
	ServerAddr string // required, e.g. "cloud.example.com"
	ServerPort int    // default 7000
	Token      string // shared secret with frps
	User       string // optional proxy-name prefix; sets ClientCommonConfig.User

	// Protocol is the FRP transport: "tcp" (default), "ws", or "wss".
	// When ws/wss is selected the agent dials the cloud's HTTPS port at
	// the hardcoded FRP path /~!frp and the cloud's WS bridge pipes the
	// upgraded conn to the loopback FRP server. Cloudflare/DO App Platform
	// only route HTTP(S) — wss is what makes the prod tunnel work.
	Protocol string
}

// TCPProxy declares one local TCP service that should be reachable from the
// frps loopback. RemotePort=0 lets frps auto-assign; we usually pin it.
type TCPProxy struct {
	Name       string
	LocalIP    string
	LocalPort  int
	RemotePort int
}

// Tunnel wraps a frpc Service. Call Run, then Close.
type Tunnel struct {
	svc *frpclient.Service
}

// NewTunnel builds the frpc client with the given proxies pre-registered
// via the in-memory ConfigSource — no config-file path involved.
func NewTunnel(tc TunnelConfig, proxies []TCPProxy) (*Tunnel, error) {
	// LoginFailExit defaults to true, which makes the agent exit if frps
	// isn't reachable on the first dial. For a long-running home-host agent
	// that needs to survive cloud restarts (and `make start` race
	// conditions), false + frp's built-in retry is what we want.
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
		// Disable FRP's app-layer TLS — Cloudflare / DO App Platform
		// already terminates TLS at the edge, and double-wrapping breaks
		// the wss handshake. HeartbeatInterval=30 is mandatory: Cloudflare
		// reaps idle WebSockets at ~100 s, App Platform at ~60 s, and FRP's
		// default heartbeat is disabled (-1) which kills the control conn.
		common.Transport.Protocol = tc.Protocol
		common.Transport.TLS.Enable = lo.ToPtr(false)
		common.Transport.HeartbeatInterval = 30
	case "", "tcp":
		// Default raw-TCP transport; nothing to set.
	default:
		return nil, fmt.Errorf("unsupported FRP protocol %q (expected tcp/websocket/wss)", tc.Protocol)
	}
	if err := common.Complete(); err != nil {
		return nil, fmt.Errorf("frp client common: %w", err)
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
	configurers = frpconfig.CompleteProxyConfigurers(configurers)

	cs := source.NewConfigSource()
	if err := cs.ReplaceAll(configurers, nil); err != nil {
		return nil, fmt.Errorf("frp client proxies: %w", err)
	}
	agg := source.NewAggregator(cs)

	svc, err := frpclient.NewService(frpclient.ServiceOptions{
		Common:                 common,
		ConfigSourceAggregator: agg,
	})
	if err != nil {
		return nil, fmt.Errorf("frp client new: %w", err)
	}
	return &Tunnel{svc: svc}, nil
}

// Run blocks until ctx is canceled or the service stops.
func (t *Tunnel) Run(ctx context.Context) error {
	return t.svc.Run(ctx)
}

// Close releases client resources.
func (t *Tunnel) Close() {
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
