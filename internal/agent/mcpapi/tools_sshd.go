package mcpapi

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// LAN sshd authorized-key tools: manage the `outpost.sshd` unit's key
// file (public-key auth for the daemon OS account) without hand-editing
// dotfiles on the host. Backed by the same admincore verbs as
// `outpost ssh authorize ...`.

type sshAuthorizeAddIn struct {
	Key string `json:"key" jsonschema:"OpenSSH public-key line (ssh-ed25519 AAAA... comment), as produced by ssh-keygen"`
}

type sshAuthorizeAddOut struct {
	OK          bool   `json:"ok"`
	Fingerprint string `json:"fingerprint"`
}

type sshAuthorizeRemoveIn struct {
	Fingerprint string `json:"fingerprint" jsonschema:"SHA256 fingerprint of the key to remove (as listed by outpost_ssh_authorize_list)"`
}

type sshAuthorizeRemoveOut struct {
	OK      bool `json:"ok"`
	Removed bool `json:"removed"`
}

type sshAuthorizeListOut struct {
	Fingerprints []string `json:"fingerprints"`
}

func (s *Server) registerSSHAuthorizeTools() {
	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_ssh_authorize_add",
		Description: "Authorize a public key for LAN SSH logins (the outpost.sshd unit): appends the key to the unit's authorized_keys file. Idempotent — re-adding a present key returns its fingerprint. Keys authenticate the daemon OS account only; password auth stays available.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in sshAuthorizeAddIn) (*mcp.CallToolResult, sshAuthorizeAddOut, error) {
		fp, err := s.core.AddSSHAuthorizedKey(in.Key)
		if err != nil {
			return apiErrResult[sshAuthorizeAddOut](err)
		}
		return nil, sshAuthorizeAddOut{OK: true, Fingerprint: fp}, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_ssh_authorize_remove",
		Description: "Remove a public key from the LAN SSH authorized_keys file, by fingerprint. Idempotent — removing an absent fingerprint reports removed=false.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in sshAuthorizeRemoveIn) (*mcp.CallToolResult, sshAuthorizeRemoveOut, error) {
		removed, err := s.core.RemoveSSHAuthorizedKey(in.Fingerprint)
		if err != nil {
			return apiErrResult[sshAuthorizeRemoveOut](err)
		}
		return nil, sshAuthorizeRemoveOut{OK: true, Removed: removed}, nil
	})

	mcp.AddTool(s.mcp, &mcp.Tool{
		Name:        "outpost_ssh_authorize_list",
		Description: "List the SHA256 fingerprints of the public keys authorized for LAN SSH logins. Fingerprints only — key material never leaves the host.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ emptyIn) (*mcp.CallToolResult, sshAuthorizeListOut, error) {
		fps, err := s.core.ListSSHAuthorizedKeys()
		if err != nil {
			return apiErrResult[sshAuthorizeListOut](err)
		}
		if fps == nil {
			fps = []string{}
		}
		return nil, sshAuthorizeListOut{Fingerprints: fps}, nil
	})
}
