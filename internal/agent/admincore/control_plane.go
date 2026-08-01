package admincore

import (
	"net"
	"strconv"
	"strings"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// Control-plane placement surface.
//
// Hosting the DKS apiserver is a PLACEMENT CHOICE, not a separate product
// (dhnt/docs/dks-control-plane-on-sphere.md): cloudbox, a rented always-on
// box, or the user's own machine. Everything needed to run a plane on your own
// host already existed — the tunnel server, the reconcilers, the node-address
// patcher — but the only way to turn it on was to hand-edit agent.json, which
// meant the feature was built and unreachable.
//
// The token needs its own verbs because it is BOTH a secret and something the
// operator must read: workers cannot join without it, exactly like k3s's
// node-token. So it is redacted in the status view and revealed only when
// asked for by name, which keeps it out of logs and screenshares that show
// `outpost status`.

// ControlPlaneParams is a partial update. A nil field means "leave alone", so
// flipping the switch does not silently reset a customized bind.
type ControlPlaneParams struct {
	Enabled  *bool   `json:"enabled,omitempty"`
	BindAddr *string `json:"bind_addr,omitempty"`
	BindPort *int    `json:"bind_port,omitempty"`
}

// ControlPlaneResult is the redacted status. TunnelToken is populated only by
// the explicit reveal/rotate paths.
type ControlPlaneResult struct {
	OK           bool   `json:"ok"`
	ControlPlane bool   `json:"control_plane"`
	BindAddr     string `json:"bind_addr"`
	BindPort     int    `json:"bind_port"`
	HasToken     bool   `json:"has_token"`
	// TunnelToken is empty unless the caller asked to reveal or rotate it.
	TunnelToken string `json:"tunnel_token,omitempty"`
	// LANExposed is true when the bind address is not loopback. Surfaced as
	// its own flag rather than left for the reader to infer from an address,
	// because "this host is accepting cluster joins from the network" is the
	// one property of this config worth noticing at a glance.
	LANExposed     bool `json:"lan_exposed"`
	RestartPending bool `json:"restart_pending"`
}

// ControlPlaneView returns the current placement config. reveal=true includes
// the tunnel token — the value an operator hands to workers.
func (s *Server) ControlPlaneView(reveal bool) (ControlPlaneResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadConfig()
	if err != nil {
		return ControlPlaneResult{}, err
	}
	return controlPlaneResult(fc, reveal), nil
}

// SetControlPlane enables or disables hosting the apiserver on this host and
// updates the tunnel server's bind.
//
// Enabling MINTS THE TOKEN as a side effect, so the operator never has a
// control plane that is on but unjoinable. Disabling does NOT delete it: a
// host toggled off and on again would otherwise invalidate every worker's
// configuration, and the token is worthless while no server is listening.
//
// Any change is restart-pending — the tunnel server is built once at boot,
// like every other listener outpost owns.
func (s *Server) SetControlPlane(p ControlPlaneParams) (ControlPlaneResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadConfig()
	if err != nil {
		return ControlPlaneResult{}, err
	}
	if fc.Cluster == nil {
		fc.Cluster = &conf.ClusterConfig{}
	}

	if p.BindAddr != nil {
		addr := strings.TrimSpace(*p.BindAddr)
		if addr != "" && net.ParseIP(addr) == nil {
			// An IP, not a hostname: this is a bind address, and a name that
			// fails to resolve at boot would take the listener down long
			// after the operator left the config screen.
			return ControlPlaneResult{}, badRequest("bind_addr must be an IP address, got %q", addr)
		}
		fc.Cluster.TunnelBindAddr = addr
	}
	if p.BindPort != nil {
		port := *p.BindPort
		if port < 0 || port > 65535 {
			return ControlPlaneResult{}, badRequest("bind_port must be 0-65535, got %d", port)
		}
		fc.Cluster.TunnelBindPort = port
	}

	changed := p.BindAddr != nil || p.BindPort != nil
	if p.Enabled != nil {
		was := fc.Cluster.ControlPlaneOn()
		on := *p.Enabled
		fc.Cluster.ControlPlane = &on
		changed = changed || was != on
		if on {
			// Mint before saving so the token lands in the same write.
			if _, err := conf.EnsureClusterTunnelToken("", fc); err != nil {
				return ControlPlaneResult{}, internalErr("%s", err.Error())
			}
		}
	}

	if err := conf.SaveFile(s.deps.ConfigPath, fc); err != nil {
		return ControlPlaneResult{}, internalErr("%s", err.Error())
	}

	out := controlPlaneResult(fc, false)
	out.RestartPending = changed && fc.AgentName != ""
	if out.RestartPending {
		s.ScheduleRestart()
	}
	return out, nil
}

// RotateControlPlaneToken mints a new tunnel token and returns it.
//
// EVERY WORKER MUST BE RECONFIGURED after this — the token authenticates the
// frpc session, so existing workers fail to re-login on their next reconnect.
// That is the point of a rotate verb (revoking a leaked credential), but it is
// destructive enough that callers should say so in their own words; this
// method will not do it implicitly as part of some other operation.
func (s *Server) RotateControlPlaneToken() (ControlPlaneResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadConfig()
	if err != nil {
		return ControlPlaneResult{}, err
	}
	if fc.Cluster == nil {
		fc.Cluster = &conf.ClusterConfig{}
	}
	fc.Cluster.TunnelToken = "" // force regeneration
	if _, err := conf.EnsureClusterTunnelToken(s.deps.ConfigPath, fc); err != nil {
		return ControlPlaneResult{}, internalErr("%s", err.Error())
	}

	out := controlPlaneResult(fc, true)
	// Only a running server needs restarting to pick the new token up.
	out.RestartPending = fc.Cluster.ControlPlaneOn() && fc.AgentName != ""
	if out.RestartPending {
		s.ScheduleRestart()
	}
	return out, nil
}

func controlPlaneResult(fc *conf.FileConfig, reveal bool) ControlPlaneResult {
	var cc *conf.ClusterConfig
	if fc != nil {
		cc = fc.Cluster
	}
	addr, port := cc.TunnelBind()
	out := ControlPlaneResult{
		OK:           true,
		ControlPlane: cc.ControlPlaneOn(),
		BindAddr:     addr,
		BindPort:     port,
		LANExposed:   !isLoopbackIP(addr),
	}
	if cc != nil {
		out.HasToken = cc.TunnelToken != ""
		if reveal {
			out.TunnelToken = cc.TunnelToken
		}
	}
	return out
}

func isLoopbackIP(addr string) bool {
	ip := net.ParseIP(addr)
	// An unparseable or unspecified address (0.0.0.0, ::) is NOT loopback.
	// Treating an unknown value as safe is the wrong default for a flag whose
	// whole job is to warn.
	return ip != nil && ip.IsLoopback()
}

// ControlPlaneWorkerHint renders the one line an operator needs on a worker.
// Kept next to the config it describes so the two cannot drift.
func ControlPlaneWorkerHint(addr string, port int) string {
	return "point the worker's tunnel at " + net.JoinHostPort(addr, strconv.Itoa(port)) +
		" using this token"
}
