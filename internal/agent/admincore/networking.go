package admincore

import (
	"strings"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// NetworkingParams is the partial-update shape for SetNetworking. All
// fields are pointers / nil-able so the caller can change one knob
// without resetting the others. Pass an explicit empty string to clear
// a field (revert to env / hardcoded default).
type NetworkingParams struct {
	// LocalAddr — bind for the matrix-tunnel ingress. Empty to clear.
	// Use *string so callers can distinguish "leave alone" (nil) from
	// "clear to default" (pointer to "").
	LocalAddr *string `json:"local_addr,omitempty"`
	// VNCAddr — upstream for the /desktop bridge.
	VNCAddr *string `json:"vnc_addr,omitempty"`
	// AdminAddr — bind for the admin UI + MCP listener.
	AdminAddr *string `json:"admin_addr,omitempty"`
	// AdminUsers — when non-nil, replaces the entire allowlist. Pass
	// an empty slice to revert to the legacy "anyone with the OS
	// password is admin" mode.
	AdminUsers *[]string `json:"admin_users,omitempty"`
}

// NetworkingResult reports what changed. RestartPending is true
// whenever any field was modified — the listener bind addresses and
// the admin-users allowlist all take effect at boot only.
type NetworkingResult struct {
	OK             bool `json:"ok"`
	RestartPending bool `json:"restart_pending"`
}

// SetNetworking applies the partial update to the persisted
// FileConfig and (if anything changed and the host is paired)
// schedules a restart so the new listener bind / allowlist takes
// effect. First-time-setup hosts (AgentName empty) skip the restart —
// nothing is mounted yet, so a save is harmless.
func (s *Server) SetNetworking(p NetworkingParams) (NetworkingResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadConfig()
	if err != nil {
		return NetworkingResult{}, err
	}
	changed := false
	if p.LocalAddr != nil && strings.TrimSpace(*p.LocalAddr) != fc.LocalAddr {
		fc.LocalAddr = strings.TrimSpace(*p.LocalAddr)
		changed = true
	}
	if p.VNCAddr != nil && strings.TrimSpace(*p.VNCAddr) != fc.VNCAddr {
		fc.VNCAddr = strings.TrimSpace(*p.VNCAddr)
		changed = true
	}
	if p.AdminAddr != nil && strings.TrimSpace(*p.AdminAddr) != fc.AdminAddr {
		fc.AdminAddr = strings.TrimSpace(*p.AdminAddr)
		changed = true
	}
	if p.AdminUsers != nil {
		cleaned := cleanAdminUsers(*p.AdminUsers)
		if !stringSlicesEqual(cleaned, fc.AdminUsers) {
			fc.AdminUsers = cleaned
			changed = true
		}
	}
	if !changed {
		return NetworkingResult{OK: true}, nil
	}
	if err := conf.SaveFile(s.deps.ConfigPath, fc); err != nil {
		return NetworkingResult{}, internalErr("%s", err.Error())
	}
	restart := fc.AgentName != ""
	if restart {
		s.ScheduleRestart()
	}
	return NetworkingResult{OK: true, RestartPending: restart}, nil
}

// cleanAdminUsers normalizes the input: trims whitespace, drops empty
// entries, lowercases (email addresses are case-insensitive). Returns
// a fresh slice; nil input becomes an empty slice so JSON encodes as
// []. Order is preserved — the operator's listed order is meaningful
// for human readability, even if the lookup is unordered.
func cleanAdminUsers(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, e := range in {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" || seen[e] {
			continue
		}
		seen[e] = true
		out = append(out, e)
	}
	return out
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
