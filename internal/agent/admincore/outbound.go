package admincore

import (
	"strings"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/conf"
)

// ListOutbound returns the live state of every registered outbound
// mount. When no manager is wired (unpaired host), returns an empty
// slice instead of nil so JSON renders as a list.
func (s *Server) ListOutbound() []agent.OutboundView {
	return s.outboundList()
}

// UpsertOutbound validates the params, refuses collisions with local
// app names and other listener-binding mounts, persists to FileConfig,
// and re-registers the live OutboundManager so the change takes effect
// without a restart.
func (s *Server) UpsertOutbound(p OutboundParams) error {
	if s.deps.Outbound == nil {
		return unavailable("outbound manager not configured (outpost not paired?)")
	}
	if err := ValidateOutbound(&p); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadConfig()
	if err != nil {
		return err
	}
	// Local-app and outbound names share the same NoRoute namespace —
	// refuse to register an outbound that would shadow a local app.
	for _, ac := range fc.Apps {
		if strings.EqualFold(ac.Name, p.Path) {
			return conflict("path %q collides with custom app of the same name", p.Path)
		}
	}
	newCfg := conf.OutboundConfig{
		Path:       p.Path,
		Name:       p.Name,
		Host:       p.Host,
		User:       p.User,
		Scheme:     p.Scheme,
		LocalPort:  p.LocalPort,
		TTLSeconds: p.TTLSeconds,
	}
	// A listener-binding outbound (tcp or ssh) MUST NOT collide on
	// LocalPort with any other listener-binding outbound — both would
	// race to bind 127.0.0.1:<port>.
	if newCfg.BindsListener() {
		for _, ob := range fc.Outbound {
			if ob.Path == newCfg.Path {
				continue
			}
			if ob.BindsListener() && ob.LocalPort == newCfg.LocalPort {
				return conflict("local_port %d already used by outbound %q", newCfg.LocalPort, ob.Path)
			}
		}
	}
	replaced := false
	for i, ob := range fc.Outbound {
		if ob.Path == p.Path {
			fc.Outbound[i] = newCfg
			replaced = true
			break
		}
	}
	if !replaced {
		fc.Outbound = append(fc.Outbound, newCfg)
	}
	if err := conf.SaveFile(s.deps.ConfigPath, fc); err != nil {
		return internalErr("%s", err.Error())
	}
	s.deps.Outbound.Register(fc.Outbound)
	return nil
}

// DeleteOutbound removes an outbound mount by path. Idempotent — no
// error when the path doesn't exist.
func (s *Server) DeleteOutbound(path string) error {
	if s.deps.Outbound == nil {
		return unavailable("outbound manager not configured")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadConfig()
	if err != nil {
		return err
	}
	filtered := fc.Outbound[:0]
	for _, ob := range fc.Outbound {
		if ob.Path != path {
			filtered = append(filtered, ob)
		}
	}
	fc.Outbound = filtered
	if err := conf.SaveFile(s.deps.ConfigPath, fc); err != nil {
		return internalErr("%s", err.Error())
	}
	s.deps.Outbound.Register(fc.Outbound)
	return nil
}

// ConnectOutbound runs the cloudbox elevate flow for the named mount
// using the supplied OS password and starts the matrix_elev pinger.
// Returns 404 when the path is unknown.
func (s *Server) ConnectOutbound(path, password string) error {
	if s.deps.Outbound == nil {
		return unavailable("outbound manager not configured")
	}
	if !s.deps.Outbound.Has(path) {
		return notFound("unknown outbound path")
	}
	if err := s.deps.Outbound.Connect(path, password); err != nil {
		return upstream("%s", err.Error())
	}
	return nil
}

// DisconnectOutbound drops the matrix_elev cookie for the named mount.
// Idempotent.
func (s *Server) DisconnectOutbound(path string) error {
	if s.deps.Outbound == nil {
		return unavailable("outbound manager not configured")
	}
	s.deps.Outbound.Disconnect(path)
	return nil
}
