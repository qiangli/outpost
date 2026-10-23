package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/service"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/discovery"
	"github.com/qiangli/outpost/internal/agent/hostauth"
	"github.com/qiangli/outpost/internal/agent/peerhosts"
	"github.com/qiangli/outpost/internal/agent/svcunit"
)

// The LAN sshd as an in-process capability (unit `provider: internal`,
// `owner: outpost`, `capability: outpost.sshd`).
//
// This is the always-on, daemon-managed variant of `outpost sshd`: the same
// in-process SSH server (shell, exec, SFTP/scp, port forwarding) on a plain
// TCP port bound to all interfaces, with the same persistent host key — but
// started, stopped and health-checked by the service reconciler instead of
// a foreground terminal, so it survives logout and reboot with the daemon
// (which supervisord keeps up).
//
// Auth accepts the OS password (as today) AND public keys from the unit's
// authorized_keys file (default `<config-dir>/sshd_authorized_keys`,
// OpenSSH format; `settings.authorized_keys` overrides). Keys authenticate
// the daemon OS account only — enforced by the server's PublicKeyCallback.
//
// The port lives in the DECLARATION (`ports: [{port: 2222, bind: lan}]`),
// default 2222; `settings.port` is honored as a fallback so
// `--service-set sshd.settings.port=...` does what it says. mDNS
// advertisement of the lan-ssh endpoint is on by default
// (`settings.mdns=false` opts out) and does NOT require discovery_enabled.
type sshdCapability struct {
	// Daemon carries the live objects only the boot path has. Resolved
	// once at construction; a config change re-execs the daemon, so the
	// capability never needs to re-read them.
	Daemon sshdDaemonDeps

	// Firewall installs the Windows inbound allow rule. Nil selects the
	// platform default (netsh on Windows, no-op elsewhere); tests inject
	// a fake.
	Firewall sshdFirewall

	// Advertise publishes the mDNS record. Nil selects
	// discovery.Advertise; tests inject a recording fake.
	Advertise sshdAdvertiseFunc

	// BindHost overrides the bind interface. Empty (production) binds
	// all interfaces; tests pin 127.0.0.1.
	BindHost string

	mu       sync.Mutex
	ln       net.Listener
	cancel   context.CancelFunc
	serveErr chan error
	adv      sshdAdvertiser
	applied  string
	fwErr    error
	port     int
}

// sshdDaemonDeps are the daemon boot objects the LAN listener serves with.
// Mirrors what the retired ssh_listen_addr wiring threaded from main.go.
type sshdDaemonDeps struct {
	HostKeyLoader func() (ssh.Signer, error)
	Auth          hostauth.Authenticator
	AuthURL       string
	Apps          *agent.AppRegistry
	AllowLocal    bool
	AllowRemote   bool
	AllowAgent    bool
	SFTP          bool
	Peers         *peerhosts.Registry
	Sockets       []string
	CloudboxBase  string
	CloudboxProto string
	AccessToken   string
	SelfName      string
	// Advertise metadata for the mDNS record.
	AgentName        string
	AssignedHostname string
	OSUsername       string
	OAuth2Email      string
	Version          string
	Paired           bool
}

// DefaultSSHDPort is the port the unit takes when the declaration names
// none. The conventional alternate-sshd port, matching what `outpost ssh`
// tries for direct dials.
const DefaultSSHDPort = 2222

// sshdAdvertiser is the mDNS record, closed on Deactivate.
type sshdAdvertiser interface{ Close() error }

// sshdAdvertiseFunc publishes one mDNS record. Matches discovery.Advertise.
type sshdAdvertiseFunc func(ctx context.Context, opts discovery.AdvertiseOptions) (sshdAdvertiser, error)

func defaultSSHDAdvertise(ctx context.Context, opts discovery.AdvertiseOptions) (sshdAdvertiser, error) {
	return discovery.Advertise(ctx, opts)
}

// sshdSettings are the capability's knobs, read off the declaration.
type sshdSettings struct {
	Port           int
	AuthorizedKeys string
	MDNS           bool
}

func sshdSettingsOf(cfg service.Config) sshdSettings {
	s := sshdSettings{MDNS: true}
	if v := configPort(cfg, "", 0); v > 0 {
		s.Port = v
	} else if v := int(settingInt(cfg, "port", 0)); v > 0 {
		s.Port = v
	} else {
		s.Port = DefaultSSHDPort
	}
	s.AuthorizedKeys = strings.TrimSpace(setting(cfg, "authorized_keys"))
	s.MDNS = settingBoolDefault(cfg, "mdns", true)
	return s
}

func (s sshdSettings) key() string {
	return fmt.Sprintf("port=%d|ak=%s|mdns=%v", s.Port, s.AuthorizedKeys, s.MDNS)
}

func (c *sshdCapability) Activate(ctx context.Context, cfg service.Config) error {
	st := sshdSettingsOf(cfg)
	if st.Port <= 0 || st.Port > 65535 {
		return fmt.Errorf("sshd: invalid port %d", st.Port)
	}
	ak := st.AuthorizedKeys
	if ak == "" {
		var err error
		if ak, err = conf.SSHDAuthorizedKeysPath(); err != nil {
			return fmt.Errorf("sshd: authorized_keys path: %w", err)
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	// Idempotent re-activation (the reconciler re-activates on every pass
	// while degraded): when the same declaration is already serving, only
	// re-probe the firewall — never rebind a working listener. A still-
	// failing firewall returns the degraded condition (not nil) so the
	// unit stays degraded instead of flapping active/degraded.
	if c.ln != nil && c.applied == st.key() {
		if ferr := c.ensureFirewallLocked(st.Port); ferr != nil {
			c.fwErr = svcunit.NewDegraded(ferr.Error())
			return c.fwErr
		}
		c.fwErr = nil
		return nil
	}
	if c.ln != nil {
		c.stopLocked()
	}

	loader := c.Daemon.HostKeyLoader
	if loader == nil {
		loader = agent.LoadOrCreateHostKey
	}
	hostKey, err := loader()
	if err != nil {
		return fmt.Errorf("sshd: host key: %w", err)
	}

	host := c.BindHost // "" = all interfaces: LAN reachability is the point.
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", st.Port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("sshd: listen on %s: %w", addr, err)
	}
	boundPort := ln.Addr().(*net.TCPAddr).Port

	auth := c.Daemon.Auth
	if auth == nil {
		auth = hostauth.DefaultAuthenticator()
	}
	deps := agent.Deps{
		Apps:                  c.Daemon.Apps,
		Auth:                  auth,
		AuthURL:               c.Daemon.AuthURL,
		SSHAllowLocalForward:  c.Daemon.AllowLocal,
		SSHAllowRemoteForward: c.Daemon.AllowRemote,
		SSHAllowAgentForward:  c.Daemon.AllowAgent,
		SFTPEnabled:           c.Daemon.SFTP,
		SSHHostKey:            hostKey,
		SSHAuthorizedKeysFile: ak,
		PeerHosts:             c.Daemon.Peers,
		SSHForwardSockets:     c.Daemon.Sockets,
		CloudboxBase:          c.Daemon.CloudboxBase,
		CloudboxProtocol:      c.Daemon.CloudboxProto,
		AccessToken:           c.Daemon.AccessToken,
		SelfName:              c.Daemon.SelfName,
	}
	serveCtx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		errc <- agent.ServeLANSSH(serveCtx, ln, deps)
		close(errc)
	}()

	c.ln, c.cancel, c.serveErr = ln, cancel, errc
	c.applied, c.port = st.key(), boundPort
	slog.Info("sshd: listening", "addr", ln.Addr().String(), "authorized_keys", ak)

	// Firewall: best-effort gate, never fatal to the listener. A failure
	// records the exact reason and returns the degraded condition — a
	// silently unreachable listener is the failure this unit exists to
	// prevent.
	if ferr := c.ensureFirewallLocked(boundPort); ferr != nil {
		slog.Warn("sshd: firewall rule not ensured (continuing degraded)", "err", ferr)
		c.fwErr = svcunit.NewDegraded(ferr.Error())
	} else {
		c.fwErr = nil
	}

	// mDNS: best-effort too (a LAN that blocks multicast just means the
	// operator dials by address), but on by default — discoverability is
	// a gap this unit closes, not an opt-in.
	if st.MDNS {
		c.advertiseLocked(serveCtx, hostKey, boundPort)
	}
	return c.fwErr
}

// ensureFirewallLocked installs (or converges) the inbound allow rule.
// Nil firewall on non-Windows platforms is a no-op.
func (c *sshdCapability) ensureFirewallLocked(port int) error {
	fw := c.Firewall
	if fw == nil {
		fw = defaultSSHDFirewall()
	}
	if fw == nil {
		return nil
	}
	program, err := sshdProgramPath()
	if err != nil {
		return fmt.Errorf("firewall: executable path: %w", err)
	}
	if err := fw.Ensure(port, program); err != nil {
		return fmt.Errorf("firewall: %w", err)
	}
	return nil
}

// advertiseLocked publishes the lan-ssh endpoint over mDNS. A failure only
// logs — the listener serves regardless.
func (c *sshdCapability) advertiseLocked(ctx context.Context, hostKey ssh.Signer, port int) {
	adv := c.Advertise
	if adv == nil {
		adv = defaultSSHDAdvertise
	}
	instance := strings.TrimSpace(c.Daemon.AssignedHostname)
	if instance == "" {
		instance = strings.TrimSpace(c.Daemon.AgentName)
	}
	if instance == "" {
		instance = "outpost"
	}
	osUser := strings.TrimSpace(c.Daemon.OSUsername)
	if osUser == "" {
		osUser, _ = hostauth.CurrentUser()
	}
	boundAddr := c.ln.Addr().String()
	a, err := adv(ctx, discovery.AdvertiseOptions{
		InstanceName:     instance,
		Port:             port,
		PeerID:           discovery.PeerID(ssh.FingerprintSHA256(hostKey.PublicKey())),
		AgentName:        c.Daemon.AgentName,
		AssignedHostname: instance,
		OSUsername:       osUser,
		OAuth2Email:      c.Daemon.OAuth2Email,
		Version:          c.Daemon.Version,
		Paired:           c.Daemon.Paired,
		SSHListenAddr:    boundAddr,
	})
	if err != nil {
		slog.Warn("sshd: mdns advertise failed (continuing; dial by address)", "err", err)
		return
	}
	c.adv = a
	slog.Info("sshd: advertising lan-ssh over mDNS", "instance", instance, "port", port)
}

// stopLocked tears the listener and the mDNS record down. Idempotent.
func (c *sshdCapability) stopLocked() {
	if c.adv != nil {
		_ = c.adv.Close()
		c.adv = nil
	}
	if c.cancel != nil {
		c.cancel()
		c.cancel = nil
	}
	if c.ln != nil {
		_ = c.ln.Close()
		c.ln = nil
	}
	c.serveErr = nil
	c.applied = ""
	c.port = 0
}

// Configure rebinds: the port, bind and key file are fixed at listen time.
func (c *sshdCapability) Configure(context.Context, service.Config) error {
	return service.ErrRestartRequired
}

func (c *sshdCapability) Deactivate(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopLocked()
	// Withdraw the firewall rule best-effort: the listener (the actual
	// surface) is down either way, and a failed removal must not mark an
	// otherwise clean disable as failed.
	if fw := c.Firewall; fw != nil {
		if err := fw.Remove(); err != nil {
			slog.Warn("sshd: firewall rule removal failed (rule may linger)", "err", err)
		}
	} else if fw := defaultSSHDFirewall(); fw != nil {
		if err := fw.Remove(); err != nil {
			slog.Warn("sshd: firewall rule removal failed (rule may linger)", "err", err)
		}
	}
	return nil
}

func (c *sshdCapability) Health(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ln == nil {
		return fmt.Errorf("sshd: not activated")
	}
	select {
	case err, ok := <-c.serveErr:
		if !ok {
			return fmt.Errorf("sshd: listener stopped serving")
		}
		if err == nil {
			err = fmt.Errorf("sshd: listener stopped serving")
		}
		return err
	default:
	}
	// Re-probe the firewall so a repaired environment (admin granted
	// later, rule restored) heals without a restart — and a broken one
	// degrades with the reason instead of going silently unreachable.
	if ferr := c.ensureFirewallLocked(c.port); ferr != nil {
		c.fwErr = svcunit.NewDegraded(ferr.Error())
		return c.fwErr
	}
	c.fwErr = nil
	return nil
}

// boundPort is the port the listener actually took, 0 when inactive.
func (c *sshdCapability) boundPort() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.port
}

// sshdProgramPath is the executable path the Windows firewall rule scopes
// to. Symlinks resolved so the rule survives a self-upgrade rename.
func sshdProgramPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, rerr := filepath.EvalSymlinks(exe); rerr == nil {
		exe = resolved
	}
	return exe, nil
}

// newSSHDCapability builds the unit's capability from the daemon boot
// objects. Peers may be nil (unpaired): the server then keeps the
// loopback-only direct-tcpip posture, same as the standalone `outpost
// sshd` on an unpaired host.
func newSSHDCapability(fc *conf.FileConfig, cfg *conf.Config, peers *peerhosts.Registry, apps *agent.AppRegistry) *sshdCapability {
	c := &sshdCapability{}
	if fc != nil {
		c.Daemon.AllowLocal = fc.SSHAllowLocalForwardOn()
		c.Daemon.AllowRemote = fc.SSHAllowRemoteForwardOn()
		c.Daemon.AllowAgent = fc.SSHAllowAgentForwardOn()
		c.Daemon.SFTP = fc.SFTPOn()
		c.Daemon.Sockets = fc.SSHForwardSockets
		c.Daemon.CloudboxBase = cloudboxHTTPBase(fc)
		c.Daemon.AccessToken = fc.AccessToken
		c.Daemon.AssignedHostname = fc.EffectiveAssignedHostname()
		c.Daemon.OSUsername = strings.TrimSpace(fc.OSUsername)
		c.Daemon.OAuth2Email = fc.OAuth2Email
		c.Daemon.Paired = fc.AccessToken != ""
	}
	if cfg != nil {
		c.Daemon.AuthURL = cfg.AuthURL
		c.Daemon.CloudboxProto = cfg.Protocol
		c.Daemon.SelfName = cfg.AgentName
		c.Daemon.AgentName = cfg.AgentName
	}
	c.Daemon.HostKeyLoader = agent.LoadOrCreateHostKey
	c.Daemon.Peers = peers
	c.Daemon.Apps = apps
	c.Daemon.Version = agent.ReadBuildInfo().Short()
	if c.Daemon.OSUsername == "" {
		c.Daemon.OSUsername, _ = hostauth.CurrentUser()
	}
	return c
}

// defaultSSHDUnit is the declaration ensureSSHDUnit writes when no unit
// declares the sshd capability yet: enabled by default, declared port
// 2222 on all interfaces.
//
// The seeds live in yoke (OUTPOST cannot add one there), so outpost owns
// this declaration in the host-local ring — the same ring `bashy service`
// reads, so both sides see one unit.
func defaultSSHDUnit() fleet.Service {
	return fleet.Service{
		Name:        "sshd",
		Kind:        fleet.KindService,
		Description: "LAN SSH server (OpenSSH replacement): shell, exec, SFTP/scp and port forwarding on a declared port, with key + password auth",
		Provider:    fleet.ServiceProviderInternal,
		Capability:  capSSHD,
		Owner:       fleet.ServiceOwnerOutpost,
		Settings: map[string]string{
			"mdns": "true",
		},
		Ports: []fleet.ServicePort{
			{Name: "ssh", Port: DefaultSSHDPort, Bind: "lan", Protocol: "tcp"},
		},
		Enabled: true,
	}
}

// ensureSSHDUnit makes sure the sshd unit is declared, enabling it by
// default on install/register/upgrade. Missing-only and never failing:
// an existing declaration (an operator's disable, a custom port) is left
// alone, and a write or validation failure only logs — the daemon must
// boot without LAN ssh before it refuses to boot at all.
func ensureSSHDUnit(store *svcunit.Store) {
	if store == nil {
		return
	}
	units, _ := store.All()
	for _, u := range units {
		if u.IsRetired() {
			continue
		}
		if u.ProviderName() == fleet.ServiceProviderInternal && u.Capability == capSSHD {
			return // declared (in any ring): desired state is the operator's.
		}
	}
	if err := store.Save(defaultSSHDUnit()); err != nil {
		slog.Warn("sshd: could not ensure the sshd unit declaration (LAN ssh stays off)", "err", err)
		return
	}
	slog.Info("sshd: unit declared and enabled", "port", DefaultSSHDPort)
}
