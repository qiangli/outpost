package conf

import "time"

// CloudboxPairing is the identity record of a pair-once pairing with the
// cloudbox master. Token is the host token (scope `host`); it authenticates
// this host to the master and is what `outpost unpair` revokes.
type CloudboxPairing struct {
	PortalURL  string    `json:"portal_url"`
	Host       string    `json:"host"`
	Token      string    `json:"token"`
	Scopes     []string  `json:"scopes,omitempty"`
	OwnerEmail string    `json:"owner_email,omitempty"`
	PairedAt   time.Time `json:"paired_at,omitempty"`
}

// SatelliteAttachment is one satellite this host is attached to. Token is
// the satellite host token (scope `satellite-host`) the satellite issued at
// POST /api/v1/hosts/attach. Tunnel is the matrix-tunnel registration the
// satellite returned with it; the daemon dials it (see ApplyAttachmentTunnel).
type SatelliteAttachment struct {
	SatelliteID string            `json:"satellite_id"`
	Name        string            `json:"name,omitempty"`
	Endpoint    string            `json:"endpoint"`
	Host        string            `json:"host,omitempty"`
	Token       string            `json:"token"`
	Scopes      []string          `json:"scopes,omitempty"`
	AttachedAt  time.Time         `json:"attached_at,omitempty"`
	Tunnel      *AttachmentTunnel `json:"tunnel,omitempty"`
}

// AttachmentTunnel is the `registration` object of the satellite's attach
// response: the same fields /api/register/exchange returns, so the existing
// tunnel client serves an attachment exactly like a legacy pairing.
type AttachmentTunnel struct {
	AgentName            string `json:"agent_name"`
	ServerAddr           string `json:"server_addr"`
	ServerPort           int    `json:"server_port"`
	Protocol             string `json:"protocol,omitempty"`
	Token                string `json:"token,omitempty"`
	RemotePort           int    `json:"remote_port"`
	AccessToken          string `json:"access_token,omitempty"`
	CloudboxTicketPubkey string `json:"cloudbox_ticket_pubkey,omitempty"`
}

// LegacyPaired reports whether this host holds a top-level tunnel pairing
// (the pre-attach satellite pairing written by /api/register/exchange).
func (fc *FileConfig) LegacyPaired() bool {
	return fc != nil && (fc.AgentName != "" || fc.AccessToken != "")
}

// ApplyAttachmentTunnel overlays the first attachment's tunnel registration
// onto the top-level pairing fields IN MEMORY, so the daemon's existing
// tunnel/app-publishing path serves the attached satellite. A legacy
// top-level pairing wins (it is left untouched) and an attachment without a
// registration overlays nothing. SaveFile strips the overlay again, so the
// attachment never turns into a persisted legacy pairing.
func (fc *FileConfig) ApplyAttachmentTunnel() bool {
	if fc == nil || fc.LegacyPaired() || len(fc.Attachments) == 0 {
		return false
	}
	a := fc.Attachments[0]
	t := a.Tunnel
	if t == nil || t.ServerAddr == "" || t.AgentName == "" {
		return false
	}
	fc.AgentName = t.AgentName
	fc.PortalURL = a.Endpoint
	fc.ServerAddr = t.ServerAddr
	fc.ServerPort = t.ServerPort
	fc.Protocol = t.Protocol
	fc.Token = t.Token
	fc.RemotePort = t.RemotePort
	fc.AccessToken = t.AccessToken
	if fc.AccessToken == "" {
		fc.AccessToken = a.Token
	}
	if fc.CloudboxTicketPubkey == "" {
		fc.CloudboxTicketPubkey = t.CloudboxTicketPubkey
	}
	fc.attachmentOverlay = true
	return true
}

// AttachmentTunnelActive reports whether top-level pairing fields came from
// an attachment in memory rather than a legacy pairing on disk.
func (fc *FileConfig) AttachmentTunnelActive() bool {
	return fc != nil && fc.attachmentOverlay
}

// persistable returns fc as it should be written: with an attachment
// overlay (ApplyAttachmentTunnel) removed again.
func (fc *FileConfig) persistable() *FileConfig {
	if fc == nil || !fc.attachmentOverlay {
		return fc
	}
	c := *fc
	c.AgentName, c.PortalURL, c.ServerAddr, c.ServerPort = "", "", "", 0
	c.Protocol, c.Token, c.RemotePort, c.AccessToken = "", "", 0, ""
	if len(c.Attachments) > 0 && c.Attachments[0].Tunnel != nil &&
		c.CloudboxTicketPubkey == c.Attachments[0].Tunnel.CloudboxTicketPubkey {
		c.CloudboxTicketPubkey = ""
	}
	c.attachmentOverlay = false
	return &c
}

// CloudboxPaired reports whether this host holds a cloudbox identity.
func (fc *FileConfig) CloudboxPaired() bool {
	return fc != nil && fc.Cloudbox != nil && fc.Cloudbox.Token != ""
}

// LoadClientFile is LoadFile for CLIENT-side commands (ssh, connect, peers,
// reach, kubectl, ...): it applies the attachment tunnel overlay so a host
// that paired once with the master and attached to satellites reads as
// paired, the way `outpost start` already sees it. `start` keeps calling
// LoadFile + ApplyAttachmentTunnel itself, after tryReattach. The overlay
// is in-memory only; SaveFile strips it (Sprint 630 S15a).
func LoadClientFile(path string) (*FileConfig, error) {
	fc, err := LoadFile(path)
	if err != nil || fc == nil {
		return fc, err
	}
	fc.ApplyAttachmentTunnel()
	return fc, nil
}
