package admincore

import "github.com/qiangli/outpost/internal/agent/conf"

// MeshForwardOps is the mesh forwarder's operation surface. The daemon wires in
// an adapter over mesh.Forwarder (nil when the mesh data plane is off); admincore
// stays independent of the mesh package. These drive the loopback-TCP-over-mesh
// transport: Expose a local service on the worker side, Listen for a
// (peer, service) on the client/leader side.
type MeshForwardOps interface {
	Expose(service, addr string) error
	Unexpose(service string) error
	Listen(peerID, service, localAddr string) (boundAddr string, err error)
	CloseListen(addr string) error
	Forwards() MeshForwardView
}

// MeshForwardView is the live forwarder state (exposed services + listeners).
type MeshForwardView struct {
	Exposed   map[string]string  `json:"exposed"`
	Listeners []MeshListenerView `json:"listeners"`
}

// MeshListenerView describes one active forward listener.
type MeshListenerView struct {
	Addr    string `json:"addr"`
	PeerID  string `json:"peer_id"`
	Service string `json:"service"`
}

const meshOffMsg = "mesh data plane is not enabled (run `builtins set --mesh=on` on a paired host)"

// MeshExpose registers a local loopback service reachable over the mesh.
func (s *Server) MeshExpose(service, addr string) error {
	if s.deps.MeshForward == nil {
		return badRequest("%s", meshOffMsg)
	}
	if service == "" || addr == "" {
		return badRequest("service and addr are required")
	}
	return s.deps.MeshForward.Expose(service, addr)
}

// MeshUnexpose removes a service from the allowlist.
func (s *Server) MeshUnexpose(service string) error {
	if s.deps.MeshForward == nil {
		return badRequest("%s", meshOffMsg)
	}
	if service == "" {
		return badRequest("service is required")
	}
	return s.deps.MeshForward.Unexpose(service)
}

// MeshListen opens a local TCP listener forwarding to (peerID, service) over the
// mesh and returns the bound local address. localAddr "" → 127.0.0.1:0.
func (s *Server) MeshListen(peerID, service, localAddr string) (string, error) {
	if s.deps.MeshForward == nil {
		return "", badRequest("%s", meshOffMsg)
	}
	if peerID == "" || service == "" {
		return "", badRequest("peer_id and service are required")
	}
	return s.deps.MeshForward.Listen(peerID, service, localAddr)
}

// MeshCloseListen closes the forward listener bound at addr.
func (s *Server) MeshCloseListen(addr string) error {
	if s.deps.MeshForward == nil {
		return badRequest("%s", meshOffMsg)
	}
	if addr == "" {
		return badRequest("addr is required")
	}
	return s.deps.MeshForward.CloseListen(addr)
}

// MeshForwards returns the forwarder's exposed services + active listeners.
func (s *Server) MeshForwards() (MeshForwardView, error) {
	if s.deps.MeshForward == nil {
		return MeshForwardView{}, badRequest("%s", meshOffMsg)
	}
	return s.deps.MeshForward.Forwards(), nil
}

// MeshServiceView is one persistently-exposed mesh service (the wrap harness).
type MeshServiceView struct {
	Name string `json:"name"`
	Addr string `json:"addr"`
}

// MeshServiceUpsert persists a mesh service (name → loopback addr) so it is
// auto-exposed on every boot, and exposes it live now if the forwarder is up.
func (s *Server) MeshServiceUpsert(name, addr string) error {
	if name == "" || addr == "" {
		return badRequest("name and addr are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadConfig()
	if err != nil {
		return err
	}
	found := false
	for i := range fc.MeshServices {
		if fc.MeshServices[i].Name == name {
			fc.MeshServices[i].Addr = addr
			found = true
			break
		}
	}
	if !found {
		fc.MeshServices = append(fc.MeshServices, conf.MeshService{Name: name, Addr: addr})
	}
	if err := conf.SaveFile(s.deps.ConfigPath, fc); err != nil {
		return internalErr("%s", err.Error())
	}
	if s.deps.MeshForward != nil {
		if e := s.deps.MeshForward.Expose(name, addr); e != nil {
			return e
		}
	}
	return nil
}

// MeshServiceDelete removes a persisted mesh service and unexposes it live.
func (s *Server) MeshServiceDelete(name string) error {
	if name == "" {
		return badRequest("name is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadConfig()
	if err != nil {
		return err
	}
	kept := fc.MeshServices[:0]
	for _, sv := range fc.MeshServices {
		if sv.Name != name {
			kept = append(kept, sv)
		}
	}
	fc.MeshServices = kept
	if err := conf.SaveFile(s.deps.ConfigPath, fc); err != nil {
		return internalErr("%s", err.Error())
	}
	if s.deps.MeshForward != nil {
		_ = s.deps.MeshForward.Unexpose(name)
	}
	return nil
}

// MeshConsumeView is one persistent mesh consume (the dial side).
type MeshConsumeView struct {
	Service   string `json:"service"`
	PeerID    string `json:"peer_id"`
	LocalAddr string `json:"local_addr"`
}

// MeshConsumeUpsert persists a mesh consume (service ← peer id → local addr) so
// the daemon re-establishes the forward on every boot, and establishes it live
// now if the forwarder is up. Keyed by (service, local_addr) so two consumes of
// the same service on different local ports coexist.
func (s *Server) MeshConsumeUpsert(service, peerID, localAddr string) (string, error) {
	if service == "" || peerID == "" {
		return "", badRequest("service and peer_id are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadConfig()
	if err != nil {
		return "", err
	}
	found := false
	for i := range fc.MeshConsumes {
		if fc.MeshConsumes[i].Service == service && fc.MeshConsumes[i].LocalAddr == localAddr {
			fc.MeshConsumes[i].PeerID = peerID
			found = true
			break
		}
	}
	if !found {
		fc.MeshConsumes = append(fc.MeshConsumes, conf.MeshConsume{Service: service, PeerID: peerID, LocalAddr: localAddr})
	}
	if err := conf.SaveFile(s.deps.ConfigPath, fc); err != nil {
		return "", internalErr("%s", err.Error())
	}
	if s.deps.MeshForward != nil {
		return s.deps.MeshForward.Listen(peerID, service, localAddr)
	}
	return localAddr, nil
}

// MeshConsumeDelete removes a persisted mesh consume and closes its live listener.
func (s *Server) MeshConsumeDelete(service, localAddr string) error {
	if service == "" {
		return badRequest("service is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadConfig()
	if err != nil {
		return err
	}
	kept := fc.MeshConsumes[:0]
	for _, c := range fc.MeshConsumes {
		if c.Service == service && c.LocalAddr == localAddr {
			if s.deps.MeshForward != nil && c.LocalAddr != "" {
				_ = s.deps.MeshForward.CloseListen(c.LocalAddr)
			}
			continue
		}
		kept = append(kept, c)
	}
	fc.MeshConsumes = kept
	if err := conf.SaveFile(s.deps.ConfigPath, fc); err != nil {
		return internalErr("%s", err.Error())
	}
	return nil
}

// MeshConsumes lists the persisted (auto-established) mesh consumes.
func (s *Server) MeshConsumes() ([]MeshConsumeView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadConfig()
	if err != nil {
		return nil, err
	}
	out := make([]MeshConsumeView, 0, len(fc.MeshConsumes))
	for _, c := range fc.MeshConsumes {
		out = append(out, MeshConsumeView{Service: c.Service, PeerID: c.PeerID, LocalAddr: c.LocalAddr})
	}
	return out, nil
}

// MeshResolvedPeer is one peer from the cloudbox service registry.
type MeshResolvedPeer struct {
	Host     string   `json:"host"`
	PeerID   string   `json:"peer_id"`
	Services []string `json:"services"`
}

// MeshResolve returns the peers exposing the named mesh service (the registry).
func (s *Server) MeshResolve(service string) ([]MeshResolvedPeer, error) {
	if s.deps.MeshResolver == nil {
		return nil, badRequest("mesh service registry is not available (pair + enable mesh)")
	}
	return s.deps.MeshResolver(service)
}

// MeshDial resolves a peer exposing the named service and opens a local forward
// listener to it, returning the bound local address + the chosen peer host —
// the zero-config consume side ("dial git" without knowing the peer id).
func (s *Server) MeshDial(service, localAddr string) (addr, host string, err error) {
	if service == "" {
		return "", "", badRequest("service is required")
	}
	peers, e := s.MeshResolve(service)
	if e != nil {
		return "", "", e
	}
	for _, p := range peers {
		if p.PeerID == "" {
			continue
		}
		a, le := s.MeshListen(p.PeerID, service, localAddr)
		if le == nil {
			return a, p.Host, nil
		}
	}
	return "", "", badRequest("no reachable peer exposes service %q", service)
}

// MeshServices lists the persisted (auto-exposed) mesh services.
func (s *Server) MeshServices() ([]MeshServiceView, error) {
	fc, err := s.loadConfig()
	if err != nil {
		return nil, err
	}
	out := make([]MeshServiceView, 0, len(fc.MeshServices))
	for _, sv := range fc.MeshServices {
		out = append(out, MeshServiceView{Name: sv.Name, Addr: sv.Addr})
	}
	return out, nil
}
