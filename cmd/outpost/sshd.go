// `outpost sshd` — standalone LAN SSH server on port 2222.
//
// A drop-in (user-space) replacement for the system sshd, serving the
// same in-process SSH server the daemon mounts at /ssh and on
// FileConfig.SSHListenAddr — interactive shell, exec, SFTP (scp),
// port-forwarding — but as a foreground one-shot command that needs
// NO daemon, NO pairing, and NO cloudbox/internet connectivity.
//
// Use cases:
//   - A machine without sshd enabled (default macOS, most Windows
//     boxes) that you want to reach from another machine on the LAN
//     with the stock `ssh`/`scp` commands.
//   - Bootstrapping: the target machine has only the outpost binary;
//     run `outpost sshd` there, then drive the setup from a laptop
//     with `ssh -p 2222 <user>@<ip>` or `outpost ssh <user>@<ip>:2222`
//     — no internet required.
//
// Cloudbox is strictly optional: when the host happens to be paired,
// the server picks up the same extras the daemon's LAN listener gets
// (paired-peer direct-tcpip allowlist, cloudbox-tunneled `ssh -J`
// second hops). Unpaired, those simply stay off and everything else
// works.
//
// Auth is the same OS-password gate as every other outpost SSH
// surface: the username must be the OS user running `outpost sshd`,
// verified via PAM / dscl / LogonUserW. There is no cloudbox vouching
// on this listener — every connection answers the password challenge.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/discovery"
	"github.com/qiangli/outpost/internal/agent/hostauth"
	"github.com/qiangli/outpost/internal/agent/peerhosts"
)

// defaultSSHDAddr is the standalone server's default bind. All
// interfaces on purpose — the whole point of `outpost sshd` is to be
// reachable from other machines on the LAN. Port 2222 is the
// conventional alternate-sshd port and matches the default the
// `outpost ssh` client tries for direct dials.
const defaultSSHDAddr = ":2222"

func sshdCmd() *cobra.Command {
	var (
		addr   string
		noMDNS bool
	)
	cmd := &cobra.Command{
		Use:   "sshd",
		Short: "Run a standalone SSH server on the LAN (drop-in sshd, default port 2222; no pairing or cloudbox needed)",
		Long: `outpost sshd [--addr :2222]

Serves outpost's in-process SSH server (shell, exec, SFTP/scp, port
forwarding) on a plain TCP port, in the foreground, until Ctrl-C.
Works on a completely unconfigured machine: no daemon, no pairing,
no internet. Authentication is the OS-password gate — the username
must be the OS user running this command, verified via PAM (Linux),
dscl (macOS), or LogonUserW (Windows).

From another machine on the LAN:

  ssh -p 2222 <os-user>@<this-host>          # system ssh
  scp -P 2222 file <os-user>@<this-host>:    # system scp (SFTP)
  outpost ssh <os-user>@<this-host>:2222     # outpost's own client
  outpost scp -P 2222 file <this-host>:dst   # outpost's own scp

The host identity is the same persistent ed25519 key the daemon's
/ssh endpoint uses (<config-dir>/matrix/ssh_host_ed25519), so clients
that later reach this machine through cloudbox see the same host key.

When the machine happens to be paired with cloudbox, the server also
honors the paired-peer destination allowlist for ssh -J jumps; when
unpaired those extras are simply off. mDNS advertisement (on by
default; --no-mdns to disable) lets 'outpost scan' and
'outpost ssh <hostname>' on sibling machines find this server by
name instead of IP.

This command runs the listener in-process and exits with it; for an
always-on LAN listener managed by the daemon, set ssh_listen_addr
('outpost config set --ssh-listen-addr 0.0.0.0:2222') instead.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSSHD(cmd.Context(), addr, !noMDNS)
		},
	}
	cmd.Flags().StringVar(&addr, "addr", defaultSSHDAddr,
		"TCP listen address (host:port or :port). Binds all interfaces by default — this server exists to be reached over the LAN.")
	cmd.Flags().BoolVar(&noMDNS, "no-mdns", false,
		"Skip the mDNS (_outpost._tcp) advertisement; the server is then reachable by address only.")
	return cmd
}

func runSSHD(ctx context.Context, addr string, mdnsOn bool) error {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		addr = defaultSSHDAddr
	}
	if !strings.Contains(addr, ":") {
		// Bare "2222" — be forgiving, net.Listen requires the colon.
		addr = ":" + addr
	}

	// Canonicalize config/cache locations first (same one-shot
	// migration `start` performs) so the host key lands where the
	// daemon will later look for it. Best-effort: an unwritable
	// config dir should not stop a bootstrap server whose host key
	// generation below will surface the real error.
	_, _ = conf.ResolveConfigPath()

	// Config is OPTIONAL. A fresh machine with no agent.json gets an
	// empty FileConfig: every *bool toggle defaults to on
	// (forwarding, SFTP), and the cloudbox-dependent extras stay off
	// because AccessToken is empty.
	var fc *conf.FileConfig
	if cfgPath, _ := conf.DefaultConfigPath(); cfgPath != "" {
		fc, _ = conf.LoadFile(cfgPath)
	}
	if fc == nil {
		fc = &conf.FileConfig{}
	}

	hostKey, err := agent.LoadOrCreateHostKey()
	if err != nil {
		return fmt.Errorf("ssh host key: %w", err)
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("sshd: listen %s: %w", addr, err)
	}

	// Paired extras: peer-destination allowlist + cloudbox-tunneled
	// `ssh -J` second hops. All keyed off AccessToken presence — an
	// unpaired host passes nil/empty and keeps the loopback-only
	// direct-tcpip posture.
	var peers *peerhosts.Registry
	cloudboxBase := ""
	if fc.AccessToken != "" {
		peers = peerhosts.New(peerhosts.Config{
			ServerAddr: fc.ServerAddr,
			ServerPort: fc.ServerPort,
			Protocol:   fc.Protocol,
			Token:      fc.AccessToken,
		})
		cloudboxBase = cloudboxHTTPBase(fc)
	}

	deps := agent.Deps{
		Auth:                  hostauth.DefaultAuthenticator(),
		AuthURL:               fc.AuthURL,
		SSHAllowLocalForward:  fc.SSHAllowLocalForwardOn(),
		SSHAllowRemoteForward: fc.SSHAllowRemoteForwardOn(),
		SSHAllowAgentForward:  fc.SSHAllowAgentForwardOn(),
		SFTPEnabled:           fc.SFTPOn(),
		SSHHostKey:            hostKey,
		PeerHosts:             peers,
		SSHForwardSockets:     fc.SSHForwardSockets,
		CloudboxBase:          cloudboxBase,
		CloudboxProtocol:      fc.Protocol,
		AccessToken:           fc.AccessToken,
		SelfName:              fc.AgentName,
	}

	sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	osUser, _ := hostauth.CurrentUser()
	boundAddr := ln.Addr().String()
	fmt.Printf("outpost sshd: listening on %s\n", boundAddr)
	fmt.Printf("  host key:  %s\n", ssh.FingerprintSHA256(hostKey.PublicKey()))
	if osUser != "" {
		fmt.Printf("  login as:  %s (OS password)\n", osUser)
	}
	if port := portFromListenSpec(boundAddr); port > 0 {
		fmt.Printf("  connect:   ssh -p %d %s@<this-host>   |   outpost ssh %s@<this-host>:%d\n",
			port, orPlaceholder(osUser), orPlaceholder(osUser), port)
	}

	if mdnsOn {
		startSSHDAdvertise(sigCtx, fc, hostKey, addr, osUser)
	}

	err = agent.ServeLANSSH(sigCtx, ln, deps)
	if err != nil && (errors.Is(err, context.Canceled) || errors.Is(err, net.ErrClosed)) {
		err = nil
	}
	if err != nil {
		return fmt.Errorf("sshd: %w", err)
	}
	fmt.Println("outpost sshd: shut down")
	return nil
}

// startSSHDAdvertise publishes the standalone server on mDNS so
// sibling machines can find it via `outpost scan` / `outpost ssh
// <hostname>` without knowing the IP. Best-effort: a LAN that blocks
// multicast just means the operator dials by address.
func startSSHDAdvertise(ctx context.Context, fc *conf.FileConfig, hostKey ssh.Signer, addr, osUser string) {
	port := portFromListenSpec(addr)
	if port <= 0 {
		return
	}
	instance := fc.EffectiveAssignedHostname()
	if instance == "" {
		instance = "outpost"
	}
	adv, err := discovery.Advertise(ctx, discovery.AdvertiseOptions{
		InstanceName:     instance,
		Port:             port,
		PeerID:           discovery.PeerID(ssh.FingerprintSHA256(hostKey.PublicKey())),
		AgentName:        fc.AgentName,
		AssignedHostname: instance,
		OSUsername:       osUser,
		Version:          agent.ReadBuildInfo().Short(),
		Paired:           fc.AccessToken != "",
		SSHListenAddr:    addr,
	})
	if err != nil {
		slog.Warn("sshd: mdns advertise failed (continuing; dial by address)", "err", err)
		return
	}
	go func() {
		<-ctx.Done()
		_ = adv.Close()
	}()
	fmt.Printf("  mdns:      advertising as %q (_outpost._tcp) — `outpost scan` finds it\n", instance)
}

// orPlaceholder substitutes a readable placeholder for an empty
// username in the printed connect hints.
func orPlaceholder(user string) string {
	if user == "" {
		return "<os-user>"
	}
	return user
}
