package main

import (
	"path/filepath"
	"testing"

	"github.com/qiangli/outpost/internal/agent/conf"
)

// TestMergePairingPreservesDaemonAndOperatorState is the regression guard
// for the register-while-running bug: `outpost register` used to wholesale
// SaveFile the fresh exchange result, clobbering the daemon-internal
// secrets (admin_session_key / mcp_bearer_token) and any operator config
// (apps, outbound, builtins, networking, admin_users). mergePairing must
// overlay only the portal-controlled fields and preserve the rest.
func TestMergePairingPreservesDaemonAndOperatorState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.json")

	shellOff := false
	existing := &conf.FileConfig{
		// daemon-internal secrets a running daemon generated on first boot
		AdminSessionKey: []byte("session-key-keep-me"),
		MCPBearerToken:  "bearer-keep-me",
		// operator config the admin UI / CLI persisted
		Apps:         []conf.AppConfig{{Name: "ycode", Scheme: "http", Port: 8765}},
		Outbound:     []conf.OutboundConfig{{Path: "remote-pg", Scheme: "tcp", Host: "peer"}},
		AdminUsers:   []string{"liqiang@gmail.com"},
		LocalAddr:    "127.0.0.1:9999",
		ShellEnabled: &shellOff,
		// a stale prior pairing that should be overwritten
		AgentName:   "old-name",
		AccessToken: "old-token",
	}
	if err := conf.SaveFile(path, existing); err != nil {
		t.Fatalf("seed SaveFile: %v", err)
	}

	exchanged := &conf.FileConfig{
		AgentName:   "puppy",
		ServerAddr:  "ai.dhnt.io",
		ServerPort:  443,
		Protocol:    "wss",
		RemotePort:  17006,
		AccessToken: "new-token",
	}

	merged := mergePairing(path, exchanged)

	// Portal-controlled fields overlaid.
	if merged.AgentName != "puppy" || merged.AccessToken != "new-token" ||
		merged.ServerAddr != "ai.dhnt.io" || merged.ServerPort != 443 ||
		merged.Protocol != "wss" || merged.RemotePort != 17006 {
		t.Fatalf("pairing fields not overlaid: %+v", merged)
	}

	// Daemon secrets preserved — clobbering these is what broke MCP auth.
	if string(merged.AdminSessionKey) != "session-key-keep-me" {
		t.Errorf("admin_session_key clobbered: %q", merged.AdminSessionKey)
	}
	if merged.MCPBearerToken != "bearer-keep-me" {
		t.Errorf("mcp_bearer_token clobbered: %q", merged.MCPBearerToken)
	}

	// Operator config preserved.
	if len(merged.Apps) != 1 || merged.Apps[0].Name != "ycode" {
		t.Errorf("apps not preserved: %+v", merged.Apps)
	}
	if len(merged.Outbound) != 1 || merged.Outbound[0].Path != "remote-pg" {
		t.Errorf("outbound not preserved: %+v", merged.Outbound)
	}
	if len(merged.AdminUsers) != 1 || merged.AdminUsers[0] != "liqiang@gmail.com" {
		t.Errorf("admin_users not preserved: %+v", merged.AdminUsers)
	}
	if merged.LocalAddr != "127.0.0.1:9999" {
		t.Errorf("local_addr not preserved: %q", merged.LocalAddr)
	}
	if merged.ShellEnabled == nil || *merged.ShellEnabled != false {
		t.Errorf("shell_enabled toggle not preserved: %v", merged.ShellEnabled)
	}
}

// TestMergePairingFreshHost covers the installer/offline path: no config on
// disk yet, so the merge is just the exchange result verbatim.
func TestMergePairingFreshHost(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.json") // does not exist

	exchanged := &conf.FileConfig{AgentName: "fresh", AccessToken: "tok"}
	merged := mergePairing(path, exchanged)

	if merged.AgentName != "fresh" || merged.AccessToken != "tok" {
		t.Fatalf("fresh-host merge wrong: %+v", merged)
	}
	if merged.MCPBearerToken != "" || len(merged.Apps) != 0 {
		t.Fatalf("fresh-host merge invented state: %+v", merged)
	}
}

// TestMergePairingClusterCreds verifies cloudbox-issued cluster-join
// credentials are carried forward while operator-set cluster fields
// (Enabled / Mode / APIURL / NodeName) survive a re-pair.
func TestMergePairingClusterCreds(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.json")

	existing := &conf.FileConfig{
		Cluster: &conf.ClusterConfig{
			Enabled:   true,
			Mode:      "agent",
			APIURL:    "https://127.0.0.1:6443",
			NodeName:  "puppy-node",
			NodeToken: "stale-node-token",
		},
	}
	if err := conf.SaveFile(path, existing); err != nil {
		t.Fatalf("seed SaveFile: %v", err)
	}

	exchanged := &conf.FileConfig{
		AgentName: "puppy",
		Cluster: &conf.ClusterConfig{
			NodeToken:  "fresh-node-token",
			STCPSecret: "fresh-secret",
		},
	}
	merged := mergePairing(path, exchanged)

	if merged.Cluster == nil {
		t.Fatal("cluster dropped")
	}
	// Fresh creds applied.
	if merged.Cluster.NodeToken != "fresh-node-token" || merged.Cluster.STCPSecret != "fresh-secret" {
		t.Errorf("cluster creds not refreshed: %+v", merged.Cluster)
	}
	// Operator fields preserved.
	if !merged.Cluster.Enabled || merged.Cluster.Mode != "agent" ||
		merged.Cluster.APIURL != "https://127.0.0.1:6443" || merged.Cluster.NodeName != "puppy-node" {
		t.Errorf("operator cluster fields not preserved: %+v", merged.Cluster)
	}
}
