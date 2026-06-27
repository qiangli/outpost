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
