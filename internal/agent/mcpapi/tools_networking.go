package mcpapi

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qiangli/outpost/internal/agent/admincore"
)

// networkingIn mirrors admincore.NetworkingParams but uses plain
// strings + a sentinel for "leave alone" semantics: empty string means
// "do not modify". The admincore.SetNetworking handler treats explicit
// empty strings as "clear to default", so callers who want that path
// can pass the sentinel value "<clear>" instead. This keeps the JSON
// schema simple (no nested *string types the SDK can't represent
// cleanly) without losing expressiveness.
type networkingIn struct {
	LocalAddr  string   `json:"local_addr,omitempty" jsonschema:"Bind address for the matrix-tunnel ingress. Empty = leave unchanged; '<clear>' = revert to 127.0.0.1:0."`
	VNCAddr    string   `json:"vnc_addr,omitempty" jsonschema:"VNC upstream the /desktop route bridges to. Empty = leave unchanged; '<clear>' = revert to 127.0.0.1:5900."`
	AdminAddr  string   `json:"admin_addr,omitempty" jsonschema:"Bind for the admin UI + MCP listener. Empty = leave unchanged; '<clear>' = revert to 127.0.0.1:17777. Changing this requires a daemon restart."`
	AdminUsers []string `json:"admin_users,omitempty" jsonschema:"Allowlist of admin emails for the OS-auth path. Pass an empty array to revert to legacy 'anyone with OS password is admin' mode. nil = leave unchanged."`
	// SetAdminUsers explicitly flags "I'm setting admin_users to
	// exactly the supplied list" (including empty list). Without
	// this, an omitted admin_users field is treated as "leave alone".
	SetAdminUsers bool `json:"set_admin_users,omitempty" jsonschema:"Set true to apply admin_users (including the empty-list-resets-to-legacy case). Without this, an omitted/empty admin_users is treated as 'leave alone'."`

	// Wave 3A discovery knobs. Same partial-update semantics as the
	// network bind fields. Pass an explicit "<clear>" to revert a
	// string field; the bool needs SetDiscoveryEnabled=true to apply.
	SetDiscoveryEnabled     bool   `json:"set_discovery_enabled,omitempty" jsonschema:"Set true to apply discovery_enabled."`
	DiscoveryEnabled        bool   `json:"discovery_enabled,omitempty" jsonschema:"Master gate for mDNS + HTTP /discover. Requires set_discovery_enabled=true."`
	SSHListenAddr           string `json:"ssh_listen_addr,omitempty" jsonschema:"LAN TCP bind for the in-process SSH server (e.g. 0.0.0.0:2222). Empty = leave unchanged; '<clear>' = disable."`
	DiscoveryHTTPListenAddr string `json:"discovery_http_listen_addr,omitempty" jsonschema:"LAN bind for /api/v1/discover/* (e.g. 0.0.0.0:17778). Empty = leave unchanged; '<clear>' = disable."`
	PeerTrustPolicy         string `json:"peer_trust_policy,omitempty" jsonschema:"One of same-owner / same-cloudbox / tofu-allow. Empty = leave unchanged; '<clear>' = revert to same-owner default."`
}

type networkingOut struct {
	OK             bool `json:"ok"`
	RestartPending bool `json:"restart_pending"`
}

const clearSentinel = "<clear>"

func (s *Server) registerNetworkingTools() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name: "outpost_set_networking",
		Description: "Persist network bind addresses (local_addr, vnc_addr, admin_addr) " +
			"and the admin_users allowlist into the FileConfig. Each field is partial: " +
			"omit (or pass empty) to leave it unchanged, pass '<clear>' to revert to " +
			"the package default. admin_users requires set_admin_users=true to apply " +
			"(so an omitted field can't accidentally wipe the list). " +
			"All these fields take effect at boot only — the daemon restarts when " +
			"anything actually changed.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in networkingIn) (*mcp.CallToolResult, networkingOut, error) {
		params := admincore.NetworkingParams{}
		if in.LocalAddr != "" {
			v := in.LocalAddr
			if v == clearSentinel {
				v = ""
			}
			params.LocalAddr = &v
		}
		if in.VNCAddr != "" {
			v := in.VNCAddr
			if v == clearSentinel {
				v = ""
			}
			params.VNCAddr = &v
		}
		if in.AdminAddr != "" {
			v := in.AdminAddr
			if v == clearSentinel {
				v = ""
			}
			params.AdminAddr = &v
		}
		if in.SetAdminUsers {
			u := in.AdminUsers
			if u == nil {
				u = []string{}
			}
			params.AdminUsers = &u
		}
		if in.SetDiscoveryEnabled {
			v := in.DiscoveryEnabled
			params.DiscoveryEnabled = &v
		}
		if in.SSHListenAddr != "" {
			v := in.SSHListenAddr
			if v == clearSentinel {
				v = ""
			}
			params.SSHListenAddr = &v
		}
		if in.DiscoveryHTTPListenAddr != "" {
			v := in.DiscoveryHTTPListenAddr
			if v == clearSentinel {
				v = ""
			}
			params.DiscoveryHTTPListenAddr = &v
		}
		if in.PeerTrustPolicy != "" {
			v := in.PeerTrustPolicy
			if v == clearSentinel {
				v = ""
			}
			params.PeerTrustPolicy = &v
		}
		res, err := s.core.SetNetworking(params)
		if err != nil {
			return apiErrResult[networkingOut](err)
		}
		return nil, networkingOut{OK: res.OK, RestartPending: res.RestartPending}, nil
	})
}
