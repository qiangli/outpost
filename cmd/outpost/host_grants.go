package main

import (
	"strings"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/conf"
)

// hostGrantOwner is the host owner the require_login gate admits while no
// grant copy has been received: the account this host paired as. Once a
// copy arrives its owner wins (HostGrants.Allow).
func hostGrantOwner(fc *conf.FileConfig) string {
	if fc == nil || fc.Cloudbox == nil {
		return ""
	}
	return fc.Cloudbox.OwnerEmail
}

// hostGrantSources lists every server the host-grant copy can be pulled
// from: the legacy pairing (the paired server + its outpost-<host> access
// token) when the config is legacy-paired, plus one per satellite
// attachment holding an endpoint + credential.
func hostGrantSources(fc *conf.FileConfig) []agent.GrantSource {
	if fc == nil {
		return nil
	}
	var out []agent.GrantSource
	if src, ok := legacyGrantSource(fc); ok {
		out = append(out, src)
	}
	for _, a := range fc.Attachments {
		if a.Endpoint == "" || a.Token == "" {
			continue
		}
		out = append(out, agent.GrantSource{Name: a.SatelliteID, Endpoint: a.Endpoint, Token: a.Token})
	}
	return out
}

// legacyGrantSource is the legacy pairing as a grant source. The top-level
// pairing fields filled in memory from an attachment (ApplyAttachmentTunnel)
// are not a legacy pairing: that attachment is already a source.
func legacyGrantSource(fc *conf.FileConfig) (agent.GrantSource, bool) {
	if fc.AttachmentTunnelActive() || fc.AccessToken == "" {
		return agent.GrantSource{}, false
	}
	endpoint := strings.TrimSpace(fc.PortalURL)
	if endpoint == "" {
		endpoint = cloudboxHTTPBase(fc)
	}
	if endpoint == "" {
		return agent.GrantSource{}, false
	}
	return agent.GrantSource{Name: "legacy:" + fc.AgentName, Endpoint: endpoint, Token: fc.AccessToken}, true
}
