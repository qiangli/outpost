package main

import (
	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/conf"
)

// hostGrantOwner is the host owner the require_login gate admits while no
// grant copy has been received: the account this host paired as.
func hostGrantOwner(fc *conf.FileConfig) string {
	if fc == nil || fc.Cloudbox == nil {
		return ""
	}
	return fc.Cloudbox.OwnerEmail
}

// hostGrantSources lists every satellite attachment the host-grant copy
// can be pulled from (one per attachment holding an endpoint + credential).
func hostGrantSources(fc *conf.FileConfig) []agent.GrantSource {
	if fc == nil {
		return nil
	}
	var out []agent.GrantSource
	for _, a := range fc.Attachments {
		if a.Endpoint == "" || a.Token == "" {
			continue
		}
		out = append(out, agent.GrantSource{Name: a.SatelliteID, Endpoint: a.Endpoint, Token: a.Token})
	}
	return out
}
