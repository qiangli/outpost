package mcpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
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
