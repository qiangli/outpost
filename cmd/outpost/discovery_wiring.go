// Daemon-side wiring for the LAN-direct SSH listener and the LAN peer
// discovery surfaces. Kept in a separate file so `outpost start`'s
// already-long main flow doesn't grow another ~150 LOC inline.
//
// Both helpers are no-ops when the relevant FileConfig field is
// empty/off — opt-in by design (privacy + reduced attack surface
// for a default install).
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/sync/errgroup"

	"github.com/spf13/cobra"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/discovery"
	"github.com/qiangli/outpost/internal/agent/hostauth"
	"github.com/qiangli/outpost/internal/agent/peerhosts"
)

// registerDiscoveryCommands attaches the Wave 3A LAN-discovery and
// peer-assisted repair commands to the root. Called from main() right
// after the rest of the root.AddCommand block so the new commands
// stay grouped without bloating the main flow.
func registerDiscoveryCommands(root *cobra.Command) {
	root.AddCommand(
		scanCmd(),
		discoverCmd(),
		repairCmd(),
	)
}

// startLANSSHListener binds the optional LAN-direct SSH listener when
// fc.SSHListenAddr is set. Reuses the same handler chain the matrix
// tunnel /ssh endpoint uses (via agent.ServeLANSSH) so behavior stays
// in lockstep — the only difference is the cloudboxVouched gate is
// always false on LAN-direct.
func startLANSSHListener(
	gctx context.Context,
	g *errgroup.Group,
	fc *conf.FileConfig,
	cfg *conf.Config,
	sshHostKey ssh.Signer,
	peers *peerhosts.Registry,
	apps *agent.AppRegistry,
) {
	addr := strings.TrimSpace(fc.SSHListenAddr)
	if addr == "" || !fc.SSHOn() || sshHostKey == nil {
		return
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		slog.Warn("lan ssh: listen failed", "addr", addr, "err", err)
		return
	}
	slog.Info("lan ssh: listening", "addr", ln.Addr().String())

	lanDeps := agent.Deps{
		Apps:                  apps,
		Auth:                  hostauth.DefaultAuthenticator(),
		AuthURL:               cfg.AuthURL,
		SSHAllowLocalForward:  fc.SSHAllowLocalForwardOn(),
		SSHAllowRemoteForward: fc.SSHAllowRemoteForwardOn(),
		SSHAllowAgentForward:  fc.SSHAllowAgentForwardOn(),
		SFTPEnabled:           fc.SFTPOn(),
		SSHHostKey:            sshHostKey,
		PeerHosts:             peers,
		SSHForwardSockets:     fc.SSHForwardSockets,
		CloudboxBase:          cloudboxHTTPBase(fc),
		CloudboxProtocol:      cfg.Protocol,
		AccessToken:           fc.AccessToken,
		SelfName:              cfg.AgentName,
	}
	g.Go(func() error {
		if err := agent.ServeLANSSH(gctx, ln, lanDeps); err != nil &&
			!errors.Is(err, context.Canceled) &&
			!errors.Is(err, net.ErrClosed) {
			slog.Warn("lan ssh: listener exited", "err", err)
			return err
		}
		return nil
	})
}

// startDiscovery wires up the two LAN discovery surfaces when
// fc.DiscoveryOn():
//
//	1. mDNS advertisement (`<assigned_hostname>._outpost._tcp`)
//	2. HTTP /api/v1/discover/* listener (when DiscoveryHTTPListenAddr set)
//
// One gate (DiscoveryEnabled) covers both — operators decide once
// "discovery on / off" rather than juggling two flags. Default off.
func startDiscovery(
	gctx context.Context,
	g *errgroup.Group,
	fc *conf.FileConfig,
	cfg *conf.Config,
	sshHostKey ssh.Signer,
) {
	if !fc.DiscoveryOn() || sshHostKey == nil {
		return
	}
	peerID := discovery.PeerID(ssh.FingerprintSHA256(sshHostKey.PublicKey()))
	assignedHostname := fc.EffectiveAssignedHostname()

	osUsername := strings.TrimSpace(fc.OSUsername)
	if osUsername == "" {
		osUsername, _ = hostauth.CurrentUser()
	}
	version := agent.ReadBuildInfo().Short()

	// Self-PeerHello (Tier-1 metadata). Endpoints with empty Host
	// are filtered out so receivers only see actually-bound listeners.
	self := discovery.PeerHello{
		PeerID:           peerID,
		AgentName:        cfg.AgentName,
		AssignedHostname: assignedHostname,
		OAuth2Email:      fc.OAuth2Email,
		OSUsername:       osUsername,
		Version:          version,
		CloudboxBase:     cloudboxHTTPBase(fc),
		Paired:           fc.AccessToken != "",
	}
	// Split the listen-spec into Host + Port so receivers can dial
	// without re-parsing. Bind-to-all addresses (0.0.0.0, ::) fall
	// back to the assigned hostname so `<host>.local` resolution
	// carries the dial forward.
	if h, p := splitListenSpec(fc.SSHListenAddr, assignedHostname); p > 0 {
		self.Endpoints = append(self.Endpoints, discovery.Endpoint{Kind: discovery.EndpointLANSSH, Host: h, Port: p})
	}
	if h, p := splitListenSpec(fc.DiscoveryHTTPListenAddr, assignedHostname); p > 0 {
		self.Endpoints = append(self.Endpoints, discovery.Endpoint{Kind: discovery.EndpointLANHTTPDiscover, Host: h, Port: p})
	}

	// Wave 3A.1 keeps the discovery cache out-of-band: /peers returns
	// an empty list. Wave 3B wires the active-view cache here.
	peersFn := func() []discovery.Peer { return nil }

	if addr := strings.TrimSpace(fc.DiscoveryHTTPListenAddr); addr != "" {
		discoSrv := discovery.NewServer(discovery.ServerOptions{
			Self:    self,
			Signer:  sshHostKey,
			PeersFn: peersFn,
		})
		mux := http.NewServeMux()
		discoSrv.Mount(mux, "")
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			slog.Warn("discovery http: listen failed", "addr", addr, "err", err)
		} else {
			slog.Info("discovery http: listening", "addr", ln.Addr().String())
			httpSrv := &http.Server{Handler: mux}
			g.Go(func() error {
				if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
					return err
				}
				return nil
			})
			g.Go(func() error {
				<-gctx.Done()
				_ = httpSrv.Close()
				return nil
			})
		}
	}

	// mDNS advertisement. We need a port for the SRV record; prefer
	// the LAN SSH bound port (operators typically reach us by SSH),
	// fall back to the HTTP discover port.
	advPort := pickAdvertisedPort(fc)
	if advPort > 0 {
		instance := assignedHostname
		if instance == "" {
			instance = "outpost"
		}
		adv, aerr := discovery.Advertise(gctx, discovery.AdvertiseOptions{
			InstanceName:           instance,
			Port:                   advPort,
			PeerID:                 peerID,
			AgentName:              cfg.AgentName,
			AssignedHostname:       assignedHostname,
			OSUsername:             osUsername,
			OAuth2Email:            fc.OAuth2Email,
			CloudboxBase:           cloudboxHTTPBase(fc),
			Version:                version,
			Paired:                 fc.AccessToken != "",
			SSHListenAddr:          fc.SSHListenAddr,
			HTTPDiscoverListenAddr: fc.DiscoveryHTTPListenAddr,
		})
		if aerr != nil {
			slog.Warn("mdns advertise: failed", "err", aerr)
		} else {
			slog.Info("mdns advertise: started",
				"instance", instance,
				"service", discovery.ServiceName,
				"port", advPort)
			g.Go(func() error {
				<-gctx.Done()
				_ = adv.Close()
				return nil
			})
		}
	}
}

// pickAdvertisedPort returns the port to publish in the mDNS SRV
// record. Preference: SSH-listen, then HTTP-discover. Returns 0
// when neither listener is bound (no advertising in that case).
func pickAdvertisedPort(fc *conf.FileConfig) int {
	if p := portFromListenSpec(fc.SSHListenAddr); p > 0 {
		return p
	}
	if p := portFromListenSpec(fc.DiscoveryHTTPListenAddr); p > 0 {
		return p
	}
	return 0
}

// splitListenSpec splits a `host:port`, `:port`, or `port` listen-
// spec into (host, port). Bind-to-all hosts (`0.0.0.0`, `::`, `""`)
// fall back to the `<assigned-hostname>.local` form so the published
// endpoint is dialable from peers; mDNS resolves the .local to the
// caller's first non-loopback interface IP. Returns ("", 0) on
// parse failure.
func splitListenSpec(s, assignedHostname string) (string, int) {
	port := portFromListenSpec(s)
	if port == 0 {
		return "", 0
	}
	host := ""
	if i := strings.LastIndex(strings.TrimSpace(s), ":"); i >= 0 {
		host = strings.TrimSpace(s)[:i]
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		if assignedHostname != "" {
			host = assignedHostname + ".local"
		}
	}
	return host, port
}

// portFromListenSpec extracts the port from a `host:port`, `:port`,
// or `port` listen-spec. Returns 0 on parse failure.
func portFromListenSpec(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if i := strings.LastIndex(s, ":"); i >= 0 {
		s = s[i+1:]
	}
	port := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		port = port*10 + int(r-'0')
		if port > 65535 {
			return 0
		}
	}
	return port
}
