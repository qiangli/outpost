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
// POST /api/v1/hosts/attach.
type SatelliteAttachment struct {
	SatelliteID string    `json:"satellite_id"`
	Name        string    `json:"name,omitempty"`
	Endpoint    string    `json:"endpoint"`
	Token       string    `json:"token"`
	Scopes      []string  `json:"scopes,omitempty"`
	AttachedAt  time.Time `json:"attached_at,omitempty"`
}

// CloudboxPaired reports whether this host holds a cloudbox identity.
func (fc *FileConfig) CloudboxPaired() bool {
	return fc != nil && fc.Cloudbox != nil && fc.Cloudbox.Token != ""
}
