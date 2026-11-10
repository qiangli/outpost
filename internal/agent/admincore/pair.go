package admincore

import (
	"context"
	"errors"
	"fmt"
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
//
// IdentityOnly is true when the server was the cloudbox master: the
// pairing is an identity record only (FileConfig.Cloudbox), no tunnel was
// configured and nothing restarts.
type PairResult struct {
	OK             bool   `json:"ok"`
	AgentName      string `json:"agent_name"`
	RestartPending bool   `json:"restart_pending"`
	IdentityOnly   bool   `json:"identity_only,omitempty"`
	OwnerEmail     string `json:"owner_email,omitempty"`
	PortalURL      string `json:"portal_url,omitempty"`
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

	// Pair once with the cloudbox master when the server is the master;
	// a satellite / standalone server answers 404 and pairs below over the
	// legacy exchange, unchanged.
	s.mu.Lock()
	existing, err := s.loadConfig()
	s.mu.Unlock()
	if err != nil {
		return PairResult{}, err
	}
	if err := CloudboxRepairConflict(existing, server, p.Name); err != nil {
		return PairResult{}, conflict("%s", err.Error())
	}
	id, err := portal.HostPair(ctx, portal.HostPairRequest{ServerURL: server, Code: p.Code, Host: p.Name})
	if err == nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		fc, err := s.loadConfig()
		if err != nil {
			return PairResult{}, err
		}
		fc.Cloudbox = id
		if err := conf.SaveFile(s.deps.ConfigPath, fc); err != nil {
			return PairResult{}, internalErr("%s", err.Error())
		}
		return PairResult{OK: true, AgentName: id.Host, IdentityOnly: true, OwnerEmail: id.OwnerEmail, PortalURL: id.PortalURL}, nil
	}
	if !errors.Is(err, portal.ErrNoHostPair) {
		return PairResult{}, upstream("%s", err.Error())
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
	existing, err = s.loadConfig()
	if err != nil {
		return PairResult{}, err
	}
	// Remember which server issued whatever cluster join is cached before
	// the pairing fields are overlaid — the comparison below needs both.
	prevBase := CloudboxHTTPBase(existing)
	merged := *existing
	merged.AgentName = exchanged.AgentName
	merged.PortalURL = exchanged.PortalURL
	merged.ServerAddr = exchanged.ServerAddr
	merged.ServerPort = exchanged.ServerPort
	merged.Protocol = exchanged.Protocol
	merged.Token = exchanged.Token
	merged.RemotePort = exchanged.RemotePort
	merged.AuthURL = exchanged.AuthURL
	merged.AccessToken = exchanged.AccessToken
	merged.ClientOnly = exchanged.ClientOnly

	// S482: a re-pair onto a DIFFERENT server drops the previous server's
	// cached cluster join for a plain server-plane member (same fields
	// `outpost cluster clear` wipes); same-server re-pairs, peer-plane
	// members and control-plane hosts keep theirs. Mirrors
	// cmd/outpost's mergePairing.
	DropClusterJoinOnServerChange(&merged, prevBase, CloudboxHTTPBase(&merged))

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

// CloudboxRepairConflict refuses a second pair-once pairing with the
// master the host is already paired with under a different name: the host
// pairs ONCE, and a new code would leave the old record orphaned. The same
// name is left to the server (a replay is idempotent; a fresh code is
// 409 host_exists).
func CloudboxRepairConflict(fc *conf.FileConfig, server, host string) error {
	if !fc.CloudboxPaired() {
		return nil
	}
	same := strings.TrimRight(strings.TrimSpace(server), "/") == fc.Cloudbox.PortalURL
	if same && fc.Cloudbox.Host != strings.TrimSpace(host) {
		return fmt.Errorf("already paired with %s as %q; run `outpost unpair` first", fc.Cloudbox.PortalURL, fc.Cloudbox.Host)
	}
	return nil
}

// Unpair forgets every pairing this host holds. A cloudbox identity is
// deleted at the master first (DELETE /api/v1/hosts/self, which also
// detaches its satellites) and kept locally if that fails, so the host is
// never forgotten here while the master still lists it. Then the identity,
// the satellite attachments and the portal-controlled tunnel fields are
// cleared; locally-managed config (Apps, Outbound, builtins) is preserved.
// Schedules a restart so the daemon drops any tunnel and reverts to the
// unpaired admin-UI-only mode.
func (s *Server) Unpair(ctx context.Context) (PairResult, error) {
	s.mu.Lock()
	fc, err := s.loadConfig()
	s.mu.Unlock()
	if err != nil {
		return PairResult{}, err
	}
	if fc.CloudboxPaired() {
		if err := portal.HostUnpair(ctx, fc.Cloudbox.PortalURL, fc.Cloudbox.Token); err != nil {
			return PairResult{}, upstream("%s", err.Error())
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err = s.loadConfig()
	if err != nil {
		return PairResult{}, err
	}
	fc.Cloudbox = nil
	fc.Attachments = nil
	clearLegacyPairing(fc)
	if err := conf.SaveFile(s.deps.ConfigPath, fc); err != nil {
		return PairResult{}, internalErr("%s", err.Error())
	}
	s.ScheduleRestart()
	s.SetCloudbox("", "", "")
	return PairResult{OK: true, RestartPending: true}, nil
}
