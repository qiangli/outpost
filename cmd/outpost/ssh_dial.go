// Shared dial helpers for the interactive/tunnel/sftp paths in
// `outpost ssh ...`. The MCP-driven `outpost ssh exec` goes through
// admincore (which has its own chain dial); the interactive verbs
// can't easily route through MCP (Shell needs the local terminal),
// so they dial cloudbox directly using the same primitives.
//
// We deliberately do NOT duplicate admincore.dialSSHChain — that
// method depends on an admincore.Server and runs daemon-side, where
// EAUTHREQUIRED maps to a 401 APIError. The CLI path can be smarter:
// when an interactive TTY is available we prompt for the OS password
// in-process via runConnect, recovering the elev cookie without
// requiring the operator to break out and run `outpost connect`.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/discovery"
	"github.com/qiangli/outpost/internal/agent/sshclient"
)

// dialSSHTargetChain walks the (possibly hop-laden) chain rooted at
// `name`, dials each leg, and returns the innermost client plus a
// cleanup func. Uses interactive elev recovery (runConnect prompt)
// when stdin is a TTY; surfaces EAUTHREQUIRED otherwise.
func dialSSHTargetChain(ctx context.Context, name, jumpOverride string) (*sshclient.Client, func(), error) {
	chain, err := conf.ResolveSSHTargetChain(name, jumpOverride)
	if err != nil {
		return nil, nil, err
	}
	// Validate users in the chain up front.
	for _, t := range chain {
		if strings.TrimSpace(t.User) == "" {
			return nil, nil, fmt.Errorf("target %q has no user set — run `outpost ssh add %s --host %s --user <os_user>`",
				t.Name, t.Name, t.Host)
		}
	}

	cfgPath, err := conf.DefaultConfigPath()
	if err != nil {
		return nil, nil, fmt.Errorf("locate config: %w", err)
	}
	fc, err := conf.LoadFile(cfgPath)
	if err != nil {
		return nil, nil, fmt.Errorf("load config: %w", err)
	}
	if fc == nil || fc.ServerAddr == "" {
		return nil, nil, errors.New("local outpost is not paired with cloudbox yet — run `outpost register` first")
	}
	bearer := strings.TrimSpace(os.Getenv("OUTPOST_SESSION_JWT"))
	if bearer == "" {
		bearer = fc.AccessToken
	}
	if bearer == "" {
		bearer = fc.Token
	}
	if bearer == "" {
		return nil, nil, errors.New("no cloudbox bearer cached — re-pair with `outpost register`")
	}

	knownHostsPath, err := conf.KnownHostsPath()
	if err != nil {
		return nil, nil, err
	}

	clients := make([]*sshclient.Client, 0, len(chain))
	closeAll := func() {
		for i := len(clients) - 1; i >= 0; i-- {
			_ = clients[i].Close()
		}
	}

	// Leg 0: cloudbox WS dial to the outermost target, OR a plain TCP
	// dial when the target is Direct (LAN-direct path; bypasses cloudbox).
	outer := chain[0]
	var (
		outerTransport net.Conn
		dialErr        error
	)
	if outer.Direct {
		port := outer.Port
		if port <= 0 {
			port = conf.DefaultSSHPort
		}
		outerTransport, dialErr = net.DialTimeout("tcp", net.JoinHostPort(outer.Host, fmt.Sprintf("%d", port)), 30*time.Second)
		if dialErr != nil {
			return nil, nil, fmt.Errorf("lan-direct dial %s:%d: %w", outer.Host, port, dialErr)
		}
	} else {
		wsURL, err := sshclient.BuildWSURL(fc.ServerAddr, fc.ServerPort, fc.Protocol, outer.Host)
		if err != nil {
			return nil, nil, err
		}
		cookie, _ := conf.ReadSessionCookie(outer.Host)
		wsConn, werr := sshclient.DialWS(ctx, sshclient.DialOptions{
			WSURL:  wsURL,
			Bearer: bearer,
			Cookie: cookie,
			Host:   outer.Host,
			OnElevate: func(c context.Context, h string) (string, error) {
				if !haveTTY() {
					return "", errors.New("no TTY for password prompt")
				}
				fmt.Fprintf(os.Stderr, "outpost: %s requires Connect; prompting for OS password…\n", h)
				if err := runConnect(c, h, "", false, false, 0); err != nil {
					return "", err
				}
				fresh, _ := conf.ReadSessionCookie(h)
				return fresh, nil
			},
		})
		if werr != nil {
			return nil, nil, werr
		}
		outerTransport = sshclient.AsNetConn(ctx, wsConn)
	}

	hostKeyCB, err := sshclient.KnownHostsCallbackTOFU(knownHostsPath, sshclient.HostAliasForHost(outer.Host))
	if err != nil {
		_ = outerTransport.Close()
		return nil, nil, err
	}
	outerHandshakeStart := time.Now()
	outerCli, err := sshclient.Dial(ctx, sshclient.Config{
		Transport:       outerTransport,
		HostAlias:       sshclient.HostAliasForHost(outer.Host),
		User:            outer.User,
		HostKeyCallback: hostKeyCB,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("ssh handshake to %s: %w", outer.Host, err)
	}
	recordReachabilityEdge(outer, outerHandshakeStart)
	clients = append(clients, outerCli)

	// Hops 1..N: direct-tcpip → SSH layer.
	for i := 1; i < len(chain); i++ {
		hop := chain[i]
		port := hop.Port
		if port <= 0 {
			port = conf.DefaultSSHPort
		}
		hopConn, err := clients[i-1].DirectTCPIP(ctx, hop.Host, port)
		if err != nil {
			closeAll()
			return nil, nil, fmt.Errorf("direct-tcpip to %s:%d via %s: %w", hop.Host, port, chain[i-1].Name, err)
		}
		hopCB, herr := sshclient.KnownHostsCallbackTOFU(knownHostsPath, sshclient.HostAliasForHost(hop.Host))
		if herr != nil {
			_ = hopConn.Close()
			closeAll()
			return nil, nil, herr
		}
		hopHandshakeStart := time.Now()
		hopCli, err := sshclient.Dial(ctx, sshclient.Config{
			Transport:       hopConn,
			HostAlias:       sshclient.HostAliasForHost(hop.Host),
			User:            hop.User,
			HostKeyCallback: hopCB,
		})
		if err != nil {
			closeAll()
			return nil, nil, fmt.Errorf("ssh handshake to hop %s: %w", hop.Host, err)
		}
		recordReachabilityEdge(hop, hopHandshakeStart)
		clients = append(clients, hopCli)
	}
	return clients[len(clients)-1], closeAll, nil
}

// recordReachabilityEdge appends a ReachabilityEdge to the default
// reachability ledger after a successful SSH dial. Best-effort:
// failure to write is logged, never fatal. Self-PeerID derives from
// the local outpost's host key. Peer-PeerID stays empty until
// fingerprint-discovery wiring lands in Wave 3B.2; until then
// PeerName carries the target alias.
func recordReachabilityEdge(t conf.SSHTarget, started time.Time) {
	latency := time.Since(started).Milliseconds()
	var self discovery.PeerID
	if signer, err := agent.LoadOrCreateHostKey(); err == nil && signer != nil {
		self = discovery.PeerID(ssh.FingerprintSHA256(signer.PublicKey()))
	}
	transport := "cloudbox-ssh"
	if t.Direct {
		transport = "lan-direct-ssh"
	}
	port := t.Port
	if port <= 0 {
		port = conf.DefaultSSHPort
	}
	edge := discovery.ReachabilityEdge{
		Self:      self,
		PeerName:  t.Name,
		Endpoint:  discovery.Endpoint{Kind: discovery.EndpointLANSSH, Host: t.Host, Port: port},
		Transport: transport,
		LatencyMs: latency,
		At:        time.Now(),
	}
	if !t.Direct {
		edge.Endpoint.Kind = discovery.EndpointCloudboxSSH
	}
	if _, err := discovery.AppendLedgerEntry(edge); err != nil {
		slog.Debug("reachability ledger: append failed", "err", err, "peer_name", t.Name)
	}
}
