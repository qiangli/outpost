package admincore

import (
	"path/filepath"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// MirrorJobView is one continuous, mobility-aware directory-mirror job.
type MirrorJobView struct {
	Source  string `json:"source"`
	Service string `json:"service"`
	LANOnly bool   `json:"lan_only"`
}

// MirrorView is the mirror feature's read shape.
type MirrorView struct {
	Enabled bool            `json:"enabled"`
	Jobs    []MirrorJobView `json:"jobs"`
}

// Mirror returns the persisted live-mirror config.
func (s *Server) Mirror() (MirrorView, error) {
	fc, err := s.loadConfig()
	if err != nil {
		return MirrorView{}, err
	}
	v := MirrorView{}
	if fc.Mirror != nil {
		v.Enabled = fc.Mirror.Enabled
		for _, j := range fc.Mirror.Jobs {
			v.Jobs = append(v.Jobs, MirrorJobView{Source: j.Source, Service: j.Service, LANOnly: j.LANOnly})
		}
	}
	return v, nil
}

// MirrorUpsert adds (or updates) a mobility-aware mirror job keyed by source dir:
// mirror Source to the peer exposing mesh Service, only while reachable (and
// same-LAN when lanOnly). Enables the feature, persists, schedules a restart.
func (s *Server) MirrorUpsert(source, service string, lanOnly bool) error {
	if source == "" || service == "" {
		return badRequest("source and service are required")
	}
	abs, err := filepath.Abs(source)
	if err != nil {
		return badRequest("invalid source %q: %s", source, err.Error())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadConfig()
	if err != nil {
		return err
	}
	if fc.Mirror == nil {
		fc.Mirror = &conf.MirrorConfig{}
	}
	found := false
	for i := range fc.Mirror.Jobs {
		if fc.Mirror.Jobs[i].Source == abs {
			fc.Mirror.Jobs[i].Service = service
			fc.Mirror.Jobs[i].LANOnly = lanOnly
			found = true
			break
		}
	}
	if !found {
		fc.Mirror.Jobs = append(fc.Mirror.Jobs, conf.MirrorJob{Source: abs, Service: service, LANOnly: lanOnly})
	}
	fc.Mirror.Enabled = true
	if err := conf.SaveFile(s.deps.ConfigPath, fc); err != nil {
		return internalErr("%s", err.Error())
	}
	s.ScheduleRestart()
	return nil
}

// MirrorDelete removes a mirror job by source dir and schedules a restart.
// Disables the feature when the last job is removed.
func (s *Server) MirrorDelete(source string) error {
	if source == "" {
		return badRequest("source is required")
	}
	abs, _ := filepath.Abs(source)
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadConfig()
	if err != nil {
		return err
	}
	if fc.Mirror == nil {
		return nil
	}
	kept := fc.Mirror.Jobs[:0]
	for _, j := range fc.Mirror.Jobs {
		if j.Source != abs && j.Source != source {
			kept = append(kept, j)
		}
	}
	fc.Mirror.Jobs = kept
	if len(kept) == 0 {
		fc.Mirror.Enabled = false
	}
	if err := conf.SaveFile(s.deps.ConfigPath, fc); err != nil {
		return internalErr("%s", err.Error())
	}
	s.ScheduleRestart()
	return nil
}
