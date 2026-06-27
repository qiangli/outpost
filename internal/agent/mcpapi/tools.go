package mcpapi

// Phase 1 parity tools live here. Each tool is a thin wrapper that
// translates the MCP CallToolRequest's typed args into the matching
// admincore call, then renders the result as a CallToolResult.
//
// All tool names use the verb_noun convention (modelcontextprotocol
// best-practice). All tools share these conventions:
//
//   - Mutation tools return a small JSON object {ok, ...} as the tool
//     result; restart-pending operations surface restart_pending=true
//     so callers can poll outpost://status until the daemon is back.
//   - Read operations live as MCP resources (outpost://status,
//     outpost://config, outpost://apps, outpost://outbound) rather
//     than as tools — they're idempotent fetches that benefit from
//     the protocol's resource semantics.
//   - Validation errors become MCP CallToolResult.IsError=true with
//     the admincore APIError message as text. Network / internal
//     errors return ((nil), (zero), err) — the SDK translates these
//     into proper JSON-RPC errors.

func (s *Server) registerTools() {
	// Phase 1 tool registrations are batched in the dedicated files —
	// see tools_pair.go, tools_apps.go, etc. for the per-domain groups.
	// Keeping them split makes it easy to extend without one giant file.
	s.registerPairingTools()
	s.registerBuiltinsTools()
	s.registerNetworkingTools()
	s.registerAppsTools()
	s.registerOutboundTools()
	s.registerClusterTools()
	s.registerLifecycleTools()
	s.registerUpgradeTools()
	s.registerSSHTools()
	s.registerGossipTools()
	s.registerMeshTools()
	s.registerMirrorTools()
}

func (s *Server) registerResources() {
	// Resources are registered in resources.go.
	s.registerReadOnlyResources()
}
