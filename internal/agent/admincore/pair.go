package admincore

import (
	"context"
	"strings"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/portal"
)

// PairParams is the wire shape for the portal exchange.
//
//   - Server: portal URL (defaults to https://ai.dhnt.io when empty).
//   - Code: one-time pairing code from the portal (required).
//   - Name: host name to register (required).
//   - Title: optional human-readable subtitle shown in the portal.
//   - AuthURL: optional external app-level auth endpoint.
//   - ClientOnly: register as a credential-only outpost (no inbound
//     listeners, no matrix tunnel) — see register --client-only.
type PairParams struct {
	Server     string `json:"server,omitempty"`
	Code       string `json:"code"`
	Name       string `json:"name"`
	Title      string `json:"title,omitempty"`
	AuthURL    string `json:"auth_url,omitempty"`
	ClientOnly bool   `json:"client_only,omitempty"`
}

// PairResult reports the new AgentName cloudbox assigned (typically
// echoing the requested Name) plus the restart signal callers should
// poll on.
type PairResult struct {
	OK             bool   `json:"ok"`
	AgentName      string `json:"agent_name"`
	RestartPending bool   `json:"restart_pending"`
}

// Pair runs the portal exchange and merges the result into the
// persisted FileConfig (preserving locally-managed fields: Apps,
// Outbound, built-in toggles, Cluster). Schedules a restart so the new
// tunnel/identity takes effect.
func (s *Server) Pair(ctx context.Context, p PairParams) (PairResult, error) {
	if strings.TrimSpace(p.Code) == "" || strings.TrimSpace(p.Name) == "" {
		return PairResult{}, badRequest("code and name are required")
	}
	server := strings.TrimSpace(p.Server)
	if server == "" {
		server = "https://ai.dhnt.io"
	}
	exchanged, err := portal.Exchange(ctx, portal.ExchangeRequest{
		ServerURL:  server,
		Code:       p.Code,
		Name:       p.Name,
		Title:      p.Title,
		AuthURL:    p.AuthURL,
		ClientOnly: p.ClientOnly,
	})
	if err != nil {
		return PairResult{}, upstream("%s", err.Error())
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	existing, err := s.loadConfig()
	if err != nil {
		return PairResult{}, err
	}
	merged := *existing
	merged.AgentName = exchanged.AgentName
	merged.ServerAddr = exchanged.ServerAddr
	merged.ServerPort = exchanged.ServerPort
	merged.Protocol = exchanged.Protocol
	merged.Token = exchanged.Token
	merged.RemotePort = exchanged.RemotePort
	merged.AuthURL = exchanged.AuthURL
	merged.AccessToken = exchanged.AccessToken
	merged.ClientOnly = exchanged.ClientOnly

	if err := conf.SaveFile(s.deps.ConfigPath, &merged); err != nil {
		return PairResult{}, internalErr("%s", err.Error())
	}
	s.ScheduleRestart()
	// Refresh the in-memory deps snapshot so a follow-up SuggestOutbound
	// call (or any other op that reads CloudboxBase/AccessToken) sees the
	// newly-paired state without a process restart.
	s.SetCloudbox(CloudboxHTTPBase(&merged), merged.AccessToken, merged.AgentName)
	return PairResult{
		OK:             true,
		AgentName:      merged.AgentName,
		RestartPending: true,
	}, nil
}

// Unpair clears the portal-controlled fields (AgentName, Token, etc.)
// while preserving locally-managed config (Apps, Outbound, builtins).
// Schedules a restart so the daemon drops its tunnel and reverts to the
// unpaired admin-UI-only mode.
//
// New capability — the admin UI doesn't expose this today, but agents
// occasionally need to nuke a stale pairing without editing
// agent.json by hand.
func (s *Server) Unpair() (PairResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err := s.loadConfig()
	if err != nil {
		return PairResult{}, err
	}
	fc.AgentName = ""
	fc.ServerAddr = ""
	fc.ServerPort = 0
	fc.Protocol = ""
	fc.Token = ""
	fc.RemotePort = 0
	fc.AccessToken = ""
	fc.AuthURL = ""
	fc.ClientOnly = false
	if err := conf.SaveFile(s.deps.ConfigPath, fc); err != nil {
		return PairResult{}, internalErr("%s", err.Error())
	}
	s.ScheduleRestart()
	s.SetCloudbox("", "", "")
	return PairResult{OK: true, RestartPending: true}, nil
}
