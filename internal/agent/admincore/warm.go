package admincore

import "github.com/qiangli/outpost/internal/agent/conf"

// SetWarmDesired persists the DESIRED warm set (the models cloudbox last
// asked this host to keep warm). Called by the warm executor whenever a
// /admin/warm load/shard/unload changes the set, so the intent survives
// a daemon restart. Writes through the shared config mutex so it can't
// race a concurrent builtins toggle; no restart is scheduled (the live
// executor already holds the in-memory set — this is durability only).
func (s *Server) SetWarmDesired(models []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadConfig()
	if err != nil {
		return err
	}
	fc.WarmDesired = models
	if err := conf.SaveFile(s.deps.ConfigPath, fc); err != nil {
		return internalErr("%s", err.Error())
	}
	return nil
}
