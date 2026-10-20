package admincore

import (
	"context"
	"errors"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/federation"
)

// Karmada federation surface. A federation HOST publishes a Karmada control
// plane from its own DKS; the host reconciler (cmd/outpost) does the slow work
// (install, kubeconfig, CA hash, self-register) so `on` only records intent and
// restarts, and `status` reports what the reconciler last saw. The token verb
// is the only one that reveals credentials.

const federationBuiltin = "karmada"

// FederationParams is a partial update; omitted fields are left alone.
type FederationParams struct {
	ClusterName string `json:"cluster_name,omitempty"`
	APIPort     int    `json:"api_port,omitempty"`
	Kubeconfig  string `json:"kubeconfig,omitempty"`
	// Force lets `off` proceed when the built-in cannot be uninstalled.
	Force bool `json:"force,omitempty"`
}

// FederationResult never carries a secret.
type FederationResult struct {
	OK             bool                `json:"ok"`
	Enabled        bool                `json:"enabled"`
	Role           string              `json:"role,omitempty"`
	ControlPlane   bool                `json:"control_plane"`
	Phase          string              `json:"phase,omitempty"`
	Kubeconfig     string              `json:"kubeconfig,omitempty"`
	KubeconfigOK   bool                `json:"kubeconfig_present"`
	APIPort        int                 `json:"api_port,omitempty"`
	ClusterName    string              `json:"cluster_name,omitempty"`
	CAHash         string              `json:"ca_hash,omitempty"`
	FederationID   string              `json:"federation_id,omitempty"`
	SelfRegistered bool                `json:"self_registered"`
	Members        []federation.Member `json:"members,omitempty"`
	LastError      string              `json:"last_error,omitempty"`
	RestartPending bool                `json:"restart_pending"`
	Warning        string              `json:"warning,omitempty"`

	// Member role. Endpoint is the federation host's frps (not a secret).
	Endpoint     string `json:"endpoint,omitempty"`
	JoinPending  bool   `json:"join_pending,omitempty"`
	VisitorBound bool   `json:"visitor_bound,omitempty"`
	MirrorIP     string `json:"mirror_ip,omitempty"`
	AgentReady   bool   `json:"agent_ready,omitempty"`
}

// FederationTokenResult is the revealed join material for a member.
type FederationTokenResult struct {
	OK             bool   `json:"ok"`
	Endpoint       string `json:"endpoint"`
	TunnelToken    string `json:"tunnel_token"`
	STCPSecret     string `json:"stcp_secret"`
	BootstrapToken string `json:"bootstrap_token"`
	CAHash         string `json:"ca_hash"`
	FederationID   string `json:"federation_id"`
	// JoinHint names environment variables only, never literal secrets.
	JoinHint string `json:"join_hint"`
}

const federationJoinHint = "on the joining host export OUTPOST_FEDERATION_JOIN_TOKEN (tunnel_token), " +
	"OUTPOST_FEDERATION_STCP_SECRET (stcp_secret) and OUTPOST_FEDERATION_BOOTSTRAP_TOKEN (bootstrap_token), " +
	"then join with the endpoint and ca_hash above"

func federationResult(fc *conf.FileConfig, st federation.HostState) FederationResult {
	var cc *conf.ClusterConfig
	if fc != nil {
		cc = fc.Cluster
	}
	f := (*conf.FederationConfig)(nil)
	if cc != nil {
		f = cc.Federation
	}
	out := FederationResult{OK: true, Enabled: f.On(), ControlPlane: cc.ControlPlaneOn()}
	if !f.On() {
		return out
	}
	out.Role = "member"
	if f.Host {
		out.Role = "host"
	}
	out.APIPort = f.Port()
	out.ClusterName = f.ClusterName
	out.CAHash = firstNonEmpty(f.CAHash, st.CAHash)
	out.FederationID = federation.FederationID(out.CAHash)
	out.Phase = string(st.Phase)
	out.SelfRegistered = st.SelfRegistered
	out.Members = append([]federation.Member(nil), st.Members...)
	out.LastError = st.LastError
	if f.MemberOn() {
		out.Endpoint = f.Endpoint
		out.JoinPending, out.VisitorBound, out.MirrorIP, out.AgentReady = st.JoinPending, st.VisitorBound, st.MirrorIP, st.AgentReady
		return out // a member has no materialised host kubeconfig
	}
	out.Kubeconfig = f.KubeconfigPath()
	if _, err := os.Stat(out.Kubeconfig); err == nil {
		out.KubeconfigOK = true
	}
	return out
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func federationOSCheck() error {
	if runtime.GOOS == "windows" {
		return badRequest("%s", federation.ErrUnsupportedOS.Error())
	}
	return nil
}

func (s *Server) federationState() federation.HostState {
	if s.deps.FederationState == nil {
		return federation.HostState{}
	}
	return s.deps.FederationState()
}

// FederationStatus reports federation state without revealing secrets.
func (s *Server) FederationStatus() (FederationResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadConfig()
	if err != nil {
		return FederationResult{}, err
	}
	return federationResult(fc, s.federationState()), nil
}

// FederationOn makes this host a federation host. It requires a hosted control
// plane, and restarts so the plane container is recreated with the Karmada
// publisher.
func (s *Server) FederationOn(p FederationParams) (FederationResult, error) {
	if err := federationOSCheck(); err != nil {
		return FederationResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadConfig()
	if err != nil {
		return FederationResult{}, err
	}
	if fc.Cluster == nil || !fc.Cluster.ControlPlaneOn() {
		return FederationResult{}, badRequest("federation host needs a hosted control plane: run `outpost cluster control-plane on` first")
	}
	if p.APIPort < 0 || p.APIPort > 65535 {
		return FederationResult{}, badRequest("api_port must be 0-65535, got %d", p.APIPort)
	}
	if fc.Cluster.Federation.MemberOn() {
		return FederationResult{}, badRequest("this host is a federation member; run `outpost cluster federation leave` first")
	}
	f := fc.Cluster.Federation
	if f == nil {
		f = &conf.FederationConfig{}
	}
	was := f.HostOn()
	f.Enabled, f.Host = true, true
	if p.APIPort != 0 {
		f.APIPort = p.APIPort
	}
	if n := strings.TrimSpace(p.ClusterName); n != "" {
		f.ClusterName = n
	}
	if p.Kubeconfig != "" {
		f.Kubeconfig = p.Kubeconfig
	}
	if f.ClusterName == "" {
		f.ClusterName = fc.AgentName
	}
	fc.Cluster.Federation = f
	if err := conf.SaveFile(s.deps.ConfigPath, fc); err != nil {
		return FederationResult{}, internalErr("%s", err.Error())
	}
	out := federationResult(fc, s.federationState())
	out.RestartPending = !was && fc.AgentName != ""
	if out.RestartPending {
		s.ScheduleRestart()
	}
	return out, nil
}

// FederationOff uninstalls the built-in, removes the materialised kubeconfig
// and clears the block. An uninstall failure blocks unless Force is set, so a
// half-removed control plane is never silently orphaned.
func (s *Server) FederationOff(ctx context.Context, p FederationParams) (FederationResult, error) {
	s.mu.Lock()
	fc, err := s.loadConfig()
	s.mu.Unlock()
	if err != nil {
		return FederationResult{}, err
	}
	if fc.Cluster == nil || !fc.Cluster.FederationOn() {
		return federationResult(fc, s.federationState()), nil
	}
	if fc.Cluster.Federation.MemberOn() {
		return s.FederationLeave(ctx, p)
	}
	f := fc.Cluster.Federation
	warning := ""
	if f.HostOn() {
		if _, err := s.BuiltinUninstall(ctx, BuiltinUninstallParams{Name: federationBuiltin}); err != nil {
			if !p.Force {
				return FederationResult{}, badRequest("uninstalling the %s built-in failed (re-run with force to clear the config anyway): %s", federationBuiltin, err.Error())
			}
			warning = "built-in uninstall failed and was skipped: " + err.Error()
		}
	}
	kc := f.KubeconfigPath()

	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err = s.loadConfig()
	if err != nil {
		return FederationResult{}, err
	}
	if fc.Cluster != nil {
		fc.Cluster.Federation = nil
	}
	if err := conf.SaveFile(s.deps.ConfigPath, fc); err != nil {
		return FederationResult{}, internalErr("%s", err.Error())
	}
	_ = federation.Remove(kc)
	out := federationResult(fc, federation.HostState{})
	out.Warning = warning
	out.RestartPending = fc.AgentName != ""
	if out.RestartPending {
		s.ScheduleRestart()
	}
	return out, nil
}

// FederationToken mints a bootstrap token and returns everything a member needs.
// Needs a host whose CA hash is known (Karmada installed and identity learned).
func (s *Server) FederationToken(ctx context.Context) (FederationTokenResult, error) {
	if err := federationOSCheck(); err != nil {
		return FederationTokenResult{}, err
	}
	s.mu.Lock()
	fc, err := s.loadConfig()
	s.mu.Unlock()
	if err != nil {
		return FederationTokenResult{}, err
	}
	if fc.Cluster == nil || !fc.Cluster.Federation.HostOn() {
		return FederationTokenResult{}, badRequest("this host is not a federation host")
	}
	f := fc.Cluster.Federation
	hash := firstNonEmpty(f.CAHash, s.federationState().CAHash)
	if hash == "" {
		return FederationTokenResult{}, conflict("federation identity not known yet: Karmada is still installing (check `outpost cluster federation status`)")
	}
	runner := s.deps.FederationRunner
	if runner == nil {
		runner = federation.ExecRunner{}
	}
	rc, err := federation.MintBootstrapToken(ctx, runner, f.KubeconfigPath(), "24h")
	if err != nil {
		return FederationTokenResult{}, internalErr("mint bootstrap token: %s", err.Error())
	}
	addr, port := fc.Cluster.TunnelBind()
	return FederationTokenResult{
		OK:             true,
		Endpoint:       joinHostPort(addr, port),
		TunnelToken:    fc.Cluster.TunnelToken,
		STCPSecret:     fc.Cluster.STCPSecret,
		BootstrapToken: rc.Token,
		CAHash:         hash,
		FederationID:   federation.FederationID(hash),
		JoinHint:       federationJoinHint,
	}, nil
}

// RecordFederationCAHash persists a learned CA hash (called by the reconciler).
func (s *Server) RecordFederationCAHash(hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadConfig()
	if err != nil {
		return err
	}
	if fc.Cluster == nil || fc.Cluster.Federation == nil {
		return nil
	}
	if fc.Cluster.Federation.CAHash == hash {
		return nil
	}
	fc.Cluster.Federation.CAHash = hash
	return conf.SaveFile(s.deps.ConfigPath, fc)
}

func joinHostPort(addr string, port int) string {
	return net.JoinHostPort(addr, strconv.Itoa(port))
}

// FederationJoinParams is what a member needs, as printed by `federation token`.
type FederationJoinParams struct {
	Endpoint       string `json:"endpoint"`
	Token          string `json:"token"`
	STCPSecret     string `json:"stcp_secret"`
	BootstrapToken string `json:"bootstrap_token"`
	CAHash         string `json:"ca_hash"`
	ClusterName    string `json:"cluster_name,omitempty"`
}

// federationGOOS is runtime.GOOS; a var so the Windows refusal is testable.
var federationGOOS = runtime.GOOS

func (s *Server) federationPendingPath() (string, error) {
	if s.deps.FederationPendingPath != "" {
		return s.deps.FederationPendingPath, nil
	}
	return federation.DefaultPendingJoinPath()
}

// FederationJoin makes this host's plane a federation member. Everything but
// the bootstrap token goes to agent.json; the token (24 h, single use) goes to
// a 0600 pending-join file the daemon's member reconciler deletes once
// `karmadactl register` succeeds. The restart recreates the runtime container
// with the federation visitor; the reconciler then waits for 127.0.0.1:<port>,
// creates the mirror Service/Endpoints, registers and applies the fixups.
func (s *Server) FederationJoin(p FederationJoinParams) (FederationResult, error) {
	if err := federation.CheckOSFor(federationGOOS); err != nil {
		return FederationResult{}, badRequest("%s", err.Error())
	}
	endpoint, err := normalizeFederationEndpoint(p.Endpoint)
	if err != nil {
		return FederationResult{}, err
	}
	secret := strings.TrimSpace(p.STCPSecret)
	if secret == "" {
		return FederationResult{}, badRequest("stcp_secret is required")
	}
	bootstrap := strings.TrimSpace(p.BootstrapToken)
	if !federation.ValidBootstrapToken(bootstrap) {
		return FederationResult{}, badRequest("bootstrap_token must look like abcdef.0123456789abcdef")
	}
	hash := federation.NormalizeHash(p.CAHash)
	if hash == "" {
		return FederationResult{}, badRequest("ca_hash must be sha256:<64 hex>")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadConfig()
	if err != nil {
		return FederationResult{}, err
	}
	if fc.Cluster == nil || !fc.Cluster.ControlPlaneOn() {
		return FederationResult{}, badRequest("a federation member needs its own hosted control plane: run `outpost cluster control-plane on` first")
	}
	if fc.Cluster.Federation.HostOn() {
		return FederationResult{}, badRequest("this host is a federation host; run `outpost cluster federation off` before joining another federation")
	}
	path, err := s.federationPendingPath()
	if err != nil {
		return FederationResult{}, internalErr("resolve pending-join path: %s", err.Error())
	}
	if err := federation.WritePendingJoin(path, federation.PendingJoin{BootstrapToken: bootstrap}); err != nil {
		return FederationResult{}, internalErr("write pending join: %s", err.Error())
	}
	f := fc.Cluster.Federation
	if f == nil {
		f = &conf.FederationConfig{}
	}
	f.Enabled, f.Host = true, false
	f.Endpoint, f.Token, f.STCPSecret, f.CAHash = endpoint, strings.TrimSpace(p.Token), secret, hash
	if n := strings.TrimSpace(p.ClusterName); n != "" {
		f.ClusterName = n
	}
	if f.ClusterName == "" {
		f.ClusterName = fc.AgentName
	}
	fc.Cluster.Federation = f
	if err := conf.SaveFile(s.deps.ConfigPath, fc); err != nil {
		_ = federation.RemovePendingJoin(path)
		return FederationResult{}, internalErr("%s", err.Error())
	}
	out := federationResult(fc, s.federationState())
	out.JoinPending = true
	out.RestartPending = fc.AgentName != ""
	if out.RestartPending {
		s.ScheduleRestart()
	}
	return out, nil
}

// normalizeFederationEndpoint validates host[:port] (port defaults to 7000,
// the frps port). An unspecified bind address is the host's own listener, not
// something a member can dial.
func normalizeFederationEndpoint(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", badRequest("endpoint is required (the federation host's frps, host:port)")
	}
	host, port, err := net.SplitHostPort(raw)
	if err != nil {
		host, port = raw, "7000"
	}
	n, perr := strconv.Atoi(port)
	if host == "" || perr != nil || n < 1 || n > 65535 {
		return "", badRequest("endpoint %q must be host:port", raw)
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		return "", badRequest("endpoint %q is a wildcard bind address; use the federation host's LAN or mesh address", raw)
	}
	return joinHostPort(host, n), nil
}

// FederationLeave undoes a join: `karmadactl unregister` through the still-up
// visitor (removes karmada-agent and the Cluster object), removes the mirror
// Service/Endpoints, deletes any pending join and clears the block; the
// restart drops the visitor session. An unregister failure blocks unless
// Force is set.
func (s *Server) FederationLeave(ctx context.Context, p FederationParams) (FederationResult, error) {
	s.mu.Lock()
	fc, err := s.loadConfig()
	s.mu.Unlock()
	if err != nil {
		return FederationResult{}, err
	}
	if fc.Cluster == nil || !fc.Cluster.Federation.MemberOn() {
		return FederationResult{}, badRequest("this host is not a federation member")
	}
	f := fc.Cluster.Federation
	warning := ""
	path, perr := s.federationPendingPath()
	_, pending, _ := federation.ReadPendingJoin(path)
	if pending {
		// Never registered: there is nothing to unregister, but the mirror
		// may already exist.
		warning = "join was still pending; nothing was registered"
	}
	leave := s.deps.FederationLeave
	if leave == nil {
		leave = func(context.Context, string, string, bool) error {
			return errors.New("federation leave is not wired in this process")
		}
	}
	if err := leave(ctx, f.ClusterName, fc.Cluster.ControlPlaneKubeconfig, pending); err != nil {
		if !p.Force {
			return FederationResult{}, badRequest("leaving the federation failed (re-run with force to clear the config anyway): %s", err.Error())
		}
		warning = "leave failed and was skipped: " + err.Error()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err = s.loadConfig()
	if err != nil {
		return FederationResult{}, err
	}
	if fc.Cluster != nil {
		fc.Cluster.Federation = nil
	}
	if err := conf.SaveFile(s.deps.ConfigPath, fc); err != nil {
		return FederationResult{}, internalErr("%s", err.Error())
	}
	if perr == nil {
		_ = federation.RemovePendingJoin(path)
	}
	out := federationResult(fc, federation.HostState{})
	out.Warning = warning
	out.RestartPending = fc.AgentName != ""
	if out.RestartPending {
		s.ScheduleRestart()
	}
	return out, nil
}
