package admincore

import (
	"context"
	"errors"
	"strings"

	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/portal"
)

// Satellite attach (S606 S3, yoke docs/cloudbox-members-attach-wire.md §4).
// The host uses its ONE cloudbox pairing (FileConfig.Cloudbox) to ask the
// master for an attach token, redeems it at the satellite, and stores the
// satellite credentials and tunnel registrations in Attachments. The daemon
// opens a tunnel to each attached satellite at boot.

// SatelliteRow is one satellite the host may attach to, joined with the
// local attachment state.
type SatelliteRow struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Endpoint string `json:"endpoint"`
	State    string `json:"state,omitempty"`
	Attached bool   `json:"attached"`
	// Local reports that agent.json holds this satellite's credential.
	Local bool `json:"local"`
}

// SatelliteListResult is the host-token view of GET /api/v1/satellites.
type SatelliteListResult struct {
	Satellites []SatelliteRow `json:"satellites"`
}

// SatelliteAttachParams selects the satellite (id, or a unique name) to
// attach to. Migrate replaces a legacy top-level tunnel pairing with the
// attachment. DryRun returns the plan and sends nothing.
type SatelliteAttachParams struct {
	Satellite string `json:"satellite"`
	Migrate   bool   `json:"migrate,omitempty"`
	DryRun    bool   `json:"dry_run,omitempty"`
}

// SatelliteDetachParams selects the attachment to drop (id or name; empty
// = the only one). DryRun returns the plan and sends nothing.
type SatelliteDetachParams struct {
	Satellite string `json:"satellite,omitempty"`
	DryRun    bool   `json:"dry_run,omitempty"`
}

// SatelliteResult reports an attach or detach.
type SatelliteResult struct {
	OK             bool     `json:"ok"`
	DryRun         bool     `json:"dry_run,omitempty"`
	Requests       []string `json:"requests,omitempty"`
	SatelliteID    string   `json:"satellite_id,omitempty"`
	Name           string   `json:"name,omitempty"`
	Endpoint       string   `json:"endpoint,omitempty"`
	Host           string   `json:"host,omitempty"`
	Tunnel         bool     `json:"tunnel"`
	Migrated       bool     `json:"migrated,omitempty"`
	RestartPending bool     `json:"restart_pending"`
	Warnings       []string `json:"warnings,omitempty"`
}

// wireErr maps a contract error to an APIError with the same status; the
// message keeps the stable `code` (e.g. "attached_elsewhere").
func wireErr(err error) error {
	var pe *portal.APIError
	if errors.As(err, &pe) {
		return &APIError{Status: pe.Status, Msg: pe.Error()}
	}
	return upstream("%s", err.Error())
}

func (s *Server) cloudboxIdentity() (*conf.FileConfig, error) {
	s.mu.Lock()
	fc, err := s.loadConfig()
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if !fc.CloudboxPaired() {
		return nil, conflict("not paired with the cloudbox master; run `outpost register --code CODE` first")
	}
	return fc, nil
}

// SatelliteList returns the satellites this host may attach to.
func (s *Server) SatelliteList(ctx context.Context) (SatelliteListResult, error) {
	fc, err := s.cloudboxIdentity()
	if err != nil {
		return SatelliteListResult{}, err
	}
	sats, err := portal.ListSatellites(ctx, fc.Cloudbox.PortalURL, fc.Cloudbox.Token)
	if err != nil {
		return SatelliteListResult{}, wireErr(err)
	}
	out := SatelliteListResult{Satellites: make([]SatelliteRow, 0, len(sats))}
	for _, st := range sats {
		out.Satellites = append(out.Satellites, SatelliteRow{
			ID: st.ID, Name: st.Name, Endpoint: st.Endpoint, State: st.State,
			Attached: st.Attached, Local: localAttachment(fc, st.ID) >= 0,
		})
	}
	return out, nil
}

// localAttachment returns the index of the attachment matching an id or
// name, or -1.
func localAttachment(fc *conf.FileConfig, ref string) int {
	for i, a := range fc.Attachments {
		if a.SatelliteID == ref {
			return i
		}
	}
	for i, a := range fc.Attachments {
		if a.Name != "" && a.Name == ref {
			return i
		}
	}
	return -1
}

// resolveSatellite turns an id-or-name into an id: a local attachment
// first, then the master's list (exact id, else a unique name). A ref the
// list does not know is passed through as an id for the master to judge.
func (s *Server) resolveSatellite(ctx context.Context, fc *conf.FileConfig, ref string) string {
	if i := localAttachment(fc, ref); i >= 0 {
		return fc.Attachments[i].SatelliteID
	}
	sats, err := portal.ListSatellites(ctx, fc.Cloudbox.PortalURL, fc.Cloudbox.Token)
	if err != nil {
		return ref
	}
	id, n := "", 0
	for _, st := range sats {
		if st.ID == ref {
			return ref
		}
		if st.Name == ref {
			id, n = st.ID, n+1
		}
	}
	if n == 1 {
		return id
	}
	return ref
}

// SatelliteAttach adds or refreshes one satellite attachment and schedules a
// restart so the daemon opens its tunnel.
func (s *Server) SatelliteAttach(ctx context.Context, p SatelliteAttachParams) (SatelliteResult, error) {
	ref := strings.TrimSpace(p.Satellite)
	if ref == "" {
		return SatelliteResult{}, badRequest("satellite id or name is required")
	}
	fc, err := s.cloudboxIdentity()
	if err != nil {
		return SatelliteResult{}, err
	}
	if fc.LegacyPaired() && !p.Migrate {
		return SatelliteResult{}, conflict("this host has a legacy tunnel pairing (%q at %s); rerun with --migrate to replace it with the attachment", fc.AgentName, fc.ServerAddr)
	}
	master := fc.Cloudbox.PortalURL
	if p.DryRun {
		// No network on a dry run: the id is shown as given.
		id := ref
		if i := localAttachment(fc, ref); i >= 0 {
			id = fc.Attachments[i].SatelliteID
		}
		res := SatelliteResult{OK: true, DryRun: true, SatelliteID: id, Host: fc.Cloudbox.Host, Migrated: p.Migrate && fc.LegacyPaired(), Requests: []string{
			"POST " + portal.SatelliteAttachURL(master, id),
			"POST <satellite endpoint>/api/v1/hosts/attach",
		}}
		return res, nil
	}
	id := s.resolveSatellite(ctx, fc, ref)
	grant, err := portal.RequestAttach(ctx, master, fc.Cloudbox.Token, id)
	if err != nil {
		return SatelliteResult{}, wireErr(err)
	}
	att, err := portal.RedeemAttach(ctx, grant, fc.Cloudbox.Host)
	if err != nil {
		return SatelliteResult{}, wireErr(err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err = s.loadConfig()
	if err != nil {
		return SatelliteResult{}, err
	}
	migrated := p.Migrate && fc.LegacyPaired()
	if migrated {
		clearLegacyPairing(fc)
	}
	if i := localAttachment(fc, att.SatelliteID); i >= 0 {
		fc.Attachments[i] = *att
	} else {
		fc.Attachments = append(fc.Attachments, *att)
	}
	if err := conf.SaveFile(s.deps.ConfigPath, fc); err != nil {
		return SatelliteResult{}, internalErr("%s", err.Error())
	}
	s.ScheduleRestart()
	res := SatelliteResult{
		OK: true, SatelliteID: att.SatelliteID, Name: att.Name, Endpoint: att.Endpoint,
		Host: att.Host, Tunnel: att.Tunnel != nil, Migrated: migrated, RestartPending: true,
	}
	if att.Tunnel == nil {
		res.Warnings = append(res.Warnings, "the satellite returned no tunnel registration; local apps are not published until it does (re-attach to retry)")
	}
	return res, nil
}

// SatelliteDetach leaves the satellite (DELETE /api/v1/hosts/self there,
// best effort — the master revokes the credential anyway) and removes the
// assignment at the master (DELETE /api/v1/satellites/:id/attach). The
// attachment is dropped locally only once the master accepted, and the
// daemon restarts to stop the tunnel.
func (s *Server) SatelliteDetach(ctx context.Context, p SatelliteDetachParams) (SatelliteResult, error) {
	fc, err := s.cloudboxIdentity()
	if err != nil {
		return SatelliteResult{}, err
	}
	ref := strings.TrimSpace(p.Satellite)
	if ref == "" {
		if len(fc.Attachments) != 1 {
			return SatelliteResult{}, badRequest("name the satellite to detach (this host has %d attachments)", len(fc.Attachments))
		}
		ref = fc.Attachments[0].SatelliteID
	}
	idx := localAttachment(fc, ref)
	id := ref
	var att *conf.SatelliteAttachment
	if idx >= 0 {
		att = &fc.Attachments[idx]
		id = att.SatelliteID
	}
	master := fc.Cloudbox.PortalURL
	res := SatelliteResult{OK: true, SatelliteID: id, Host: fc.Cloudbox.Host}
	if att != nil {
		res.Name, res.Endpoint, res.Tunnel = att.Name, att.Endpoint, att.Tunnel != nil
	}
	if p.DryRun {
		res.DryRun = true
		if att != nil {
			res.Requests = append(res.Requests, "DELETE "+portal.HostSelfURL(att.Endpoint))
		}
		res.Requests = append(res.Requests, "DELETE "+portal.SatelliteAttachURL(master, id))
		return res, nil
	}
	if att != nil {
		if err := portal.LeaveSatellite(ctx, att.Endpoint, att.Token); err != nil {
			res.Warnings = append(res.Warnings, "satellite leave failed (the master revokes the credential within 60 s): "+err.Error())
		}
	}
	if err := portal.Detach(ctx, master, fc.Cloudbox.Token, id); err != nil {
		return SatelliteResult{}, wireErr(err)
	}
	if att == nil {
		return res, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	fc, err = s.loadConfig()
	if err != nil {
		return SatelliteResult{}, err
	}
	if i := localAttachment(fc, id); i >= 0 {
		fc.Attachments = append(fc.Attachments[:i], fc.Attachments[i+1:]...)
		if len(fc.Attachments) == 0 {
			fc.Attachments = nil
		}
		if err := conf.SaveFile(s.deps.ConfigPath, fc); err != nil {
			return SatelliteResult{}, internalErr("%s", err.Error())
		}
	}
	s.ScheduleRestart()
	res.RestartPending = true
	return res, nil
}

// clearLegacyPairing drops the portal-controlled top-level tunnel pairing
// (the fields Unpair clears), keeping the cloudbox identity, attachments
// and locally-managed config.
func clearLegacyPairing(fc *conf.FileConfig) {
	fc.AgentName = ""
	fc.PortalURL = ""
	fc.ServerAddr = ""
	fc.ServerPort = 0
	fc.Protocol = ""
	fc.Token = ""
	fc.RemotePort = 0
	fc.AccessToken = ""
	fc.AuthURL = ""
	fc.ClientOnly = false
}
