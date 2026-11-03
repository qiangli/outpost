package admincore

import "github.com/qiangli/outpost/internal/agent/conf"

// SSOSecret supplies service publication keys through the same configuration
// owner as operator mutations. Never save the daemon's startup snapshot here:
// a late service activation would otherwise undo a completed operator toggle.
func (s *Server) SSOSecret(app string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadConfig()
	if err != nil {
		return "", err
	}
	return conf.EnsureServiceSSOSecret(s.deps.ConfigPath, fc, app)
}

// FilesSigningKey lazily mints the File Browser key without allowing a late
// capability activation to write the daemon's stale startup configuration.
func (s *Server) FilesSigningKey() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadConfig()
	if err != nil {
		return nil, err
	}
	return conf.EnsureFilesSigningKey(s.deps.ConfigPath, fc)
}
