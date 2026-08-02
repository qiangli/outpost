package mcpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/qiangli/outpost/internal/agent"
	"github.com/qiangli/outpost/internal/agent/admincore"
	"github.com/qiangli/outpost/internal/agent/conf"
	"github.com/qiangli/outpost/internal/agent/vkcred"
)

// connectTestMCP opens an authenticated MCP session against a fresh server.
func connectTestMCP(t *testing.T) (*mcp.ClientSession, *Server) {
	t.Helper()
	const token = "secret-token-1234"
	httpSrv, srv := newTestMCP(t, token)
	transport := &mcp.StreamableClientTransport{
		Endpoint: httpSrv.URL,
		HTTPClient: &http.Client{Transport: &bearerRT{
			token: token,
			base:  http.DefaultTransport,
		}},
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0.0.0"}, nil)
	session, err := client.Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session, srv
}

func callJSON(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String(), res.IsError
}

// The peer-join tools have to work end-to-end over the protocol AND never
// carry a credential back — the transcript an agent keeps is exactly the place
// a leaked token would survive longest.
func TestPeerPlaneTools_JoinIsRedacted(t *testing.T) {
	const joinToken = "JOIN-TOKEN-mcp-a1b2c3"
	session, _ := connectTestMCP(t)

	body, isErr := callJSON(t, session, "outpost_cluster_peer_plane", map[string]any{})
	if isErr {
		t.Fatalf("peer_plane reported an error: %s", body)
	}
	var view struct {
		Joined   bool   `json:"joined"`
		Endpoint string `json:"endpoint"`
		HasToken bool   `json:"has_token"`
	}
	if err := json.Unmarshal([]byte(body), &view); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	if view.Joined || view.HasToken {
		t.Fatalf("fresh host reports a peer plane: %s", body)
	}

	body, isErr = callJSON(t, session, "outpost_cluster_join_peer", map[string]any{
		"endpoint":    "10.0.0.5",
		"token":       joinToken,
		"stcp_secret": "s",
		"node_token":  "K10::node:n",
	})
	if isErr {
		t.Fatalf("join_peer failed: %s", body)
	}
	if strings.Contains(body, joinToken) {
		t.Fatalf("join_peer echoed the token: %s", body)
	}
	if err := json.Unmarshal([]byte(body), &view); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	if !view.Joined || !view.HasToken || view.Endpoint != "10.0.0.5:7000" {
		t.Fatalf("join did not take: %s", body)
	}

	// The status read must stay redacted after the credential is present.
	body, _ = callJSON(t, session, "outpost_cluster_peer_plane", map[string]any{})
	if strings.Contains(body, joinToken) {
		t.Fatalf("peer_plane leaked the token: %s", body)
	}

	// So must the config resource — the shape an agent is most likely to pull
	// wholesale into its context.
	res, err := session.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "outpost://config"})
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	for _, c := range res.Contents {
		if strings.Contains(c.Text, joinToken) {
			t.Fatalf("outpost://config leaked the join token: %s", c.Text)
		}
	}

	body, isErr = callJSON(t, session, "outpost_cluster_leave_peer", map[string]any{})
	if isErr {
		t.Fatalf("leave_peer failed: %s", body)
	}
	if err := json.Unmarshal([]byte(body), &view); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	if view.Joined || view.HasToken {
		t.Fatalf("leave did not clear: %s", body)
	}
}

// A bad endpoint must come back as a tool error the agent can read, not a
// transport failure.
func TestPeerPlaneTools_ValidationError(t *testing.T) {
	session, _ := connectTestMCP(t)
	body, isErr := callJSON(t, session, "outpost_cluster_join_peer", map[string]any{
		"endpoint": "https://10.0.0.5:7000",
		"token":    "t",
	})
	if !isErr {
		t.Fatalf("a URL endpoint was accepted: %s", body)
	}
}

// The node token is only readable on the machine hosting the plane; a worker
// asking gets a pointer to the right machine rather than an empty string.
func TestNodeTokenTool_RefusesOnANonHostingHost(t *testing.T) {
	session, _ := connectTestMCP(t)
	body, isErr := callJSON(t, session, "outpost_cluster_node_token", map[string]any{})
	if !isErr {
		t.Fatalf("a non-hosting host returned a node token: %s", body)
	}
	if !strings.Contains(body, "control plane") {
		t.Errorf("error does not say where the token lives: %s", body)
	}
}

// TestControlPlaneStatusTool_NoCredentialValues verifies the status tool never
// emits token values, only has_* booleans. The literal values are seeded into
// both the config and the fake prober so their absence from the surface is meaningful.
func TestControlPlaneStatusTool_NoCredentialValues(t *testing.T) {
	const joinToken = "FAKE-JOIN-TOKEN-xyz"
	const nodeToken = "FAKE-NODE-TOKEN-abc"
	const stcpSecret = "FAKE-STCP-SECRET-def"

	// Create an httptest server and mcpapi.Server manually so we can inject
	// credentials into both the config and the prober.
	const token = "secret-token-1234"
	configPath := filepath.Join(t.TempDir(), "agent.json")
	fc := &conf.FileConfig{
		Cluster: &conf.ClusterConfig{
			JoinToken:  joinToken,
			NodeToken:  nodeToken,
			STCPSecret: stcpSecret,
		},
	}
	if err := conf.SaveFile(configPath, fc); err != nil {
		t.Fatalf("save config: %v", err)
	}

	// Build deps, inject the fake prober, then construct the core.
	deps := admincore.Deps{
		ConfigPath: configPath,
		Apps:       agent.NewAppRegistry(),
	}
	deps.ControlPlaneStatusProber = &fakeControlPlaneStatusProber{
		status: admincore.ControlPlaneStatus{
			Hosted:              true,
			ContainerExists:     true,
			ContainerRunning:    true,
			APIServerServing:    true,
			APIServerStatusCode: 200,
			Nodes:               []admincore.Node{{Name: "worker1", Ready: true}},
			JoinEndpoint:        "https://127.0.0.1:6443",
			HasJoinToken:        true,
			HasNodeToken:        true,
			HasSTCPSecret:       true,
		},
	}
	core, err := admincore.New(deps)
	if err != nil {
		t.Fatalf("new core: %v", err)
	}

	mcpSrv, err := New(Deps{Core: core, Token: token, Version: "test"})
	if err != nil {
		t.Fatalf("new mcp server: %v", err)
	}
	httpSrv := httptest.NewServer(mcpSrv.Handler())
	t.Cleanup(httpSrv.Close)

	// Connect and call the status tool.
	transport := &mcp.StreamableClientTransport{
		Endpoint: httpSrv.URL,
		HTTPClient: &http.Client{Transport: &bearerRT{
			token: token,
			base:  http.DefaultTransport,
		}},
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "v0.0.0"}, nil)
	session, err := client.Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	body, isErr := callJSON(t, session, "outpost_control_plane_status", map[string]any{})
	if isErr {
		t.Fatalf("status reported an error: %s", body)
	}

	var status controlPlaneStatusOut
	if err := json.Unmarshal([]byte(body), &status); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}

	// Verify the presence booleans are true (the prober has them set).
	if !status.HasJoinToken || !status.HasNodeToken || !status.HasSTCPSecret {
		t.Errorf("presence booleans not set: join=%v node=%v stcp=%v", status.HasJoinToken, status.HasNodeToken, status.HasSTCPSecret)
	}

	// Confirm no token VALUES in the JSON string (even though has_* are true).
	if strings.Contains(body, joinToken) || strings.Contains(body, nodeToken) || strings.Contains(body, stcpSecret) {
		t.Errorf("status output contains a credential value: %s", body)
	}

	// Also verify the config resource stays redacted.
	res, err := session.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "outpost://config"})
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	for _, c := range res.Contents {
		if strings.Contains(c.Text, joinToken) || strings.Contains(c.Text, nodeToken) || strings.Contains(c.Text, stcpSecret) {
			t.Errorf("outpost://config leaked a credential: %s", c.Text)
		}
	}
}

// The runtime selection has to survive the protocol round-trip: an agent
// driving a peer join is the caller most likely to want a vk node, and the
// arg names have to match the agent.json keys SetBuiltins already uses.
func TestPeerPlaneTools_RuntimeSelection(t *testing.T) {
	session, _ := connectTestMCP(t)

	type view struct {
		RuntimeAgent   bool     `json:"runtime_agent"`
		RuntimeVirtual []string `json:"runtime_virtual"`
	}
	decode := func(body string) view {
		t.Helper()
		var v view
		if err := json.Unmarshal([]byte(body), &v); err != nil {
			t.Fatalf("decode %q: %v", body, err)
		}
		return v
	}

	// Selecting vk-podman + vk-native on a peer plane, the case
	// dag-to-k8s-job.sh and dks-native-job.sh need. Virtual runtimes require
	// the peer-issued vk credential bundle, so the join carries one.
	vkBundle, err := vkcred.Bundle{
		CA: []byte("peer-ca"), Token: "sa-token", Namespaces: []string{"default"},
	}.Encode()
	if err != nil {
		t.Fatalf("encode vk bundle: %v", err)
	}
	body, isErr := callJSON(t, session, "outpost_cluster_join_peer", map[string]any{
		"endpoint":        "10.0.0.5",
		"token":           "t",
		"vk_bundle":       vkBundle,
		"cluster_agent":   false,
		"cluster_virtual": []string{"vk-podman", "vk-native"},
	})
	if isErr {
		t.Fatalf("join_peer with a runtime selection failed: %s", body)
	}
	got := decode(body)
	if got.RuntimeAgent {
		t.Errorf("agent runtime selected despite cluster_agent=false: %s", body)
	}
	if len(got.RuntimeVirtual) != 2 || got.RuntimeVirtual[0] != "vk-podman" || got.RuntimeVirtual[1] != "vk-native" {
		t.Errorf("virtual selection = %v: %s", got.RuntimeVirtual, body)
	}

	// A later credential-only join must not reinstate the agent runtime.
	body, isErr = callJSON(t, session, "outpost_cluster_join_peer", map[string]any{"token": "rotated"})
	if isErr {
		t.Fatalf("re-join failed: %s", body)
	}
	if got = decode(body); got.RuntimeAgent || len(got.RuntimeVirtual) != 2 {
		t.Errorf("re-join clobbered the selection: %s", body)
	}

	// The status read reports it too, so an agent need not re-derive it.
	body, _ = callJSON(t, session, "outpost_cluster_peer_plane", map[string]any{})
	if got = decode(body); got.RuntimeAgent || len(got.RuntimeVirtual) != 2 {
		t.Errorf("peer_plane view = %s", body)
	}

	// An unknown backend is a readable tool error, not a persisted typo.
	body, isErr = callJSON(t, session, "outpost_cluster_join_peer", map[string]any{
		"cluster_virtual": []string{"vk-nonesuch"},
	})
	if !isErr {
		t.Fatalf("an unknown virtual runtime was accepted: %s", body)
	}
}

// The default is the regression that matters most: an agent that names no
// runtime must keep getting the agent-only node every deployed worker has.
func TestPeerPlaneTools_DefaultIsAgentOnly(t *testing.T) {
	session, _ := connectTestMCP(t)
	body, isErr := callJSON(t, session, "outpost_cluster_join_peer", map[string]any{
		"endpoint": "10.0.0.5",
		"token":    "t",
	})
	if isErr {
		t.Fatalf("join_peer failed: %s", body)
	}
	var v struct {
		RuntimeAgent   bool     `json:"runtime_agent"`
		RuntimeVirtual []string `json:"runtime_virtual"`
	}
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	if !v.RuntimeAgent || len(v.RuntimeVirtual) != 0 {
		t.Fatalf("default join is no longer agent-only: %s", body)
	}
}
